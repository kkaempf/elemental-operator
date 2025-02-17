/*
Copyright © 2022 - 2023 SUSE LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package register

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
	"github.com/sanity-io/litter"
	"gopkg.in/yaml.v1"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/dmidecode"
	"github.com/rancher/elemental-operator/pkg/hostinfo"
	"github.com/rancher/elemental-operator/pkg/log"
	"github.com/rancher/elemental-operator/pkg/plainauth"
	"github.com/rancher/elemental-operator/pkg/tpm"
)

type Client interface {
	Register(reg elementalv1.Registration, caCert []byte, state *State) ([]byte, error)
}

type authClient interface {
	Init(reg elementalv1.Registration) error
	GetName() string
	GetToken() (string, error)
	GetPubHash() (string, error)
	Authenticate(conn *websocket.Conn) error
}

var _ Client = (*client)(nil)

type client struct{}

func NewClient() Client {
	return &client{}
}

// Register attempts to register the machine with the elemental-operator.
// Registration updates will fetch and apply new labels, and update Machine annotations such as the IP address.
func (r *client) Register(reg elementalv1.Registration, caCert []byte, state *State) ([]byte, error) {
	auth, err := getAuthenticator(reg, state)
	if err != nil {
		return nil, fmt.Errorf("initializing authenticator: %w", err)
	}

	log.Infof("Connect to %s", reg.URL)

	conn, err := initWebsocketConn(reg.URL, caCert, auth)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := authenticate(conn, auth); err != nil {
		return nil, fmt.Errorf("%s authentication failed: %w", auth.GetName(), err)
	}
	log.Infof("%s authentication completed", auth.GetName())

	log.Debugf("elemental-register protocol version: %d", MsgLast)
	protoVersion, err := negotiateProtoVersion(conn)
	if err != nil {
		return nil, fmt.Errorf("failed to negotiate protocol version: %w", err)
	}
	log.Infof("Negotiated protocol version: %d", protoVersion)

	if state.IsUpdatable() {
		if protoVersion < MsgUpdate {
			return nil, errors.New("elemental-operator protocol version does not support update")
		}
		log.Debugln("Initiating registration update")
		if err := sendUpdateData(conn); err != nil {
			return nil, fmt.Errorf("failed to send update data: %w", err)
		}
		state.LastUpdate = time.Now()
	} else {
		state.InitialRegistration = time.Now()
	}

	if !reg.NoSMBIOS {
		log.Infof("Send SMBIOS data")
		if err := sendSMBIOSData(conn); err != nil {
			return nil, fmt.Errorf("failed to send SMBIOS data: %w", err)
		}

		if protoVersion >= MsgSystemData {
			log.Infof("Send system data")
			if err := sendSystemData(conn); err != nil {
				return nil, fmt.Errorf("failed to send system data: %w", err)
			}
		}
	}

	if protoVersion >= MsgAnnotations {
		log.Info("Send elemental annotations")
		if err := sendAnnotations(conn, reg); err != nil {
			return nil, fmt.Errorf("failend to send dynamic data: %w", err)
		}
	}

	log.Info("Get elemental configuration")
	if err := WriteMessage(conn, MsgGet, []byte{}); err != nil {
		return nil, fmt.Errorf("request elemental configuration: %w", err)
	}

	if protoVersion >= MsgConfig {
		return getConfig(conn)
	}

	// Support old Elemental Operator (<= v1.1.0)
	_, reader, err := conn.NextReader()
	if err != nil {
		return nil, fmt.Errorf("read elemental configuration: %w", err)
	}
	return io.ReadAll(reader)
}

func getAuthenticator(reg elementalv1.Registration, state *State) (authClient, error) {
	var auth authClient
	switch reg.Auth {
	case "tpm":
		state.EmulatedTPMSeed = tpm.GetTPMSeed(reg, state.EmulatedTPM, state.EmulatedTPMSeed)
		state.EmulatedTPM = reg.EmulateTPM
		auth = tpm.NewAuthClient(state.EmulatedTPMSeed)
	case "mac":
		auth = &plainauth.AuthClient{}
	case "sys-uuid":
		auth = &plainauth.AuthClient{}
	default:
		return nil, fmt.Errorf("unsupported authentication: %s", reg.Auth)
	}

	if err := auth.Init(reg); err != nil {
		return nil, fmt.Errorf("init %s authentication: %w", auth.GetName(), err)
	}
	return auth, nil
}

func initWebsocketConn(url string, caCert []byte, auth authClient) (*websocket.Conn, error) {
	dialer := websocket.DefaultDialer

	if len(caCert) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		pool.AppendCertsFromPEM(caCert)
		dialer.TLSClientConfig = &tls.Config{RootCAs: pool}
	}

	authToken, err := auth.GetToken()
	if err != nil {
		return nil, fmt.Errorf("cannot generate authentication token: %w", err)
	}
	authHash, err := auth.GetPubHash()
	if err != nil {
		return nil, fmt.Errorf("cannot retrieve authentication hash: %w", err)
	}

	wsURL := strings.Replace(url, "http", "ws", 1)
	log.Infof("Using %s Auth with Hash %s to dial %s", auth.GetName(), authHash, wsURL)

	header := http.Header{}
	header.Add("Authorization", authToken)

	conn, resp, err := dialer.Dial(wsURL, header)
	if err != nil {
		if resp != nil {
			if resp.StatusCode == http.StatusUnauthorized {
				data, err := io.ReadAll(resp.Body)
				if err == nil {
					return nil, errors.New(string(data))
				}
			} else {
				return nil, fmt.Errorf("%w (Status: %s)", err, resp.Status)
			}
		}
		return nil, err
	}
	log.Infof("Local Address: %s", conn.LocalAddr().String())
	_ = conn.SetWriteDeadline(time.Now().Add(RegistrationDeadlineSeconds * time.Second))
	_ = conn.SetReadDeadline(time.Now().Add(RegistrationDeadlineSeconds * time.Second))

	return conn, nil
}

func authenticate(conn *websocket.Conn, auth authClient) error {
	log.Debugf("Start %s authentication", auth.GetName())

	if err := auth.Authenticate(conn); err != nil {
		return err
	}
	msgType, _, err := ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("expecting auth reply: %w", err)
	}
	if msgType != MsgReady {
		return fmt.Errorf("expecting '%v' but got '%v'", MsgReady, msgType)
	}
	return nil
}

func negotiateProtoVersion(conn *websocket.Conn) (MessageType, error) {
	// Send the version of the communication protocol we support. Old operator (before kubebuilder rework)
	// will not even reply and will tear down the connection.
	data := []byte{byte(MsgLast)}
	if err := WriteMessage(conn, MsgVersion, data); err != nil {
		return MsgUndefined, err
	}

	// Retrieve the version of the communication protocol supported by the operator. This could be of help
	// to decide what we can 'ask' to the operator in future releases (we don't really do nothing with this
	// right now).
	msgType, data, err := ReadMessage(conn)
	if err != nil {
		return MsgUndefined, fmt.Errorf("communication error: %w", err)
	}
	if msgType != MsgVersion {
		return MsgUndefined, fmt.Errorf("expected msg %s, got %s", MsgVersion, msgType)
	}
	if len(data) != 1 {
		return MsgUndefined, fmt.Errorf("failed to decode protocol version, got %v (%s)", data, data)
	}
	return MessageType(data[0]), err
}

func sendSMBIOSData(conn *websocket.Conn) error {
	data, err := dmidecode.Decode()
	if err != nil {
		return errors.Wrap(err, "failed to read dmidecode data")
	}
	err = SendJSONData(conn, MsgSmbios, data)
	if err != nil {
		log.Debugf("SMBIOS data:\n%s", litter.Sdump(data))
		return err
	}
	return nil
}

func sendSystemData(conn *websocket.Conn) error {
	data, err := hostinfo.Host()
	if err != nil {
		return errors.Wrap(err, "failed to read system data")
	}
	err = SendJSONData(conn, MsgSystemData, data)
	if err != nil {
		log.Debugf("system data:\n%s", litter.Sdump(data))
		return err
	}
	return nil
}

func sendAnnotations(conn *websocket.Conn, reg elementalv1.Registration) error {
	data := map[string]string{}

	if reg.EmulateTPM {
		data["auth"] = "emulated-tpm"
	} else {
		data["auth"] = reg.Auth
	}
	if reg.NoToolkit {
		data["os.unmanaged"] = "true"
	}
	if ipAddress, err := getLocalIPAddress(conn); err != nil {
		log.Errorf("retrieving the local IP address: %w", err)
	} else {
		data["registration-ip"] = ipAddress
		log.Debugf("sending local IP: %s", data["registration-ip"])
	}
	err := SendJSONData(conn, MsgAnnotations, data)
	if err != nil {
		log.Debugf("annotation data:\n%s", litter.Sdump(data))
		return err
	}
	return nil
}

func getLocalIPAddress(conn *websocket.Conn) (string, error) {
	tcpAddr := conn.LocalAddr().String()
	idxPortNumStart := strings.LastIndexAny(tcpAddr, ":")
	if idxPortNumStart < 0 {
		return "", fmt.Errorf("Cannot understand local IP address format [%s]", tcpAddr)
	}
	return tcpAddr[0:idxPortNumStart], nil
}

func sendUpdateData(conn *websocket.Conn) error {
	if err := WriteMessage(conn, MsgUpdate, []byte{}); err != nil {
		return fmt.Errorf("sending update data: %w", err)
	}
	msgType, data, err := ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("receiving MsgUpdate response: %w", err)
	}
	switch msgType {
	case MsgError:
		msg := &ErrorMessage{}
		if err = yaml.Unmarshal(data, &msg); err != nil {
			return fmt.Errorf("decoding error-message on MsgUpdate response: %w", err)
		}
		return fmt.Errorf("update error: %s", msg.Message)
	case MsgReady:
		return nil
	default:
		return fmt.Errorf("unexpected update response message: %s", msgType)
	}
}

func getConfig(conn *websocket.Conn) ([]byte, error) {
	msgType, data, err := ReadMessage(conn)
	if err != nil {
		return nil, fmt.Errorf("read configuration response: %w", err)
	}

	log.Debugf("Got configuration response: %s", msgType.String())

	switch msgType {
	case MsgError:
		msg := &ErrorMessage{}
		if err = yaml.Unmarshal(data, &msg); err != nil {
			return nil, errors.Wrap(err, "unable to unmarshal error-message")
		}

		return nil, errors.New(msg.Message)
	case MsgConfig:
		return data, nil
	default:
		return nil, fmt.Errorf("unexpected response message: %s", msgType)
	}
}
