/*
Copyright © 2022 SUSE LLC

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

package services

import (
	"context"
	"fmt"
	"time"

	provv1 "github.com/rancher-sandbox/rancheros-operator/pkg/apis/rancheros.cattle.io/v1"
	"github.com/rancher-sandbox/rancheros-operator/pkg/clients"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UpgradeChannelSync returns a service to keep in sync managedosversions available for upgrade
func UpgradeChannelSync(interval time.Duration, namespace ...string) func(context.Context, *clients.Clients) error {
	requeuer := make(chan interface{}, 10)
	requeue := func(c *clients.Clients) {
		if len(namespace) == 0 {
			logrus.Debug("Listing all namespaces")
			err := sync(requeuer, c, "")
			if err != nil {
				logrus.Warn(err)
			}
			return
		}

		for _, n := range namespace {
			err := sync(requeuer, c, n)
			if err != nil {
				logrus.Warn(err)
			}
		}
	}
	return func(ctx context.Context, c *clients.Clients) error {
		ticker := time.NewTicker(interval)
		for {
			select {
			case <-ctx.Done():
				return fmt.Errorf("context canceled")
			case <-ticker.C:
				requeue(c)
			case <-requeuer:
				requeue(c)
			}
		}
	}
}

func sync(r chan interface{}, c *clients.Clients, namespace string) error {

	list, err := c.OS.ManagedOSVersionChannel().List(namespace, metav1.ListOptions{})
	if err != nil {
		return err
	}

	//TODO collect all errors
	versions := map[string][]provv1.ManagedOSVersion{}
	for _, cc := range list.Items {
		s, err := NewManagedOSVersionChannelSyncer(cc.Spec)
		if err != nil {
			return err
		}

		vers, err := s.sync(r, cc, c)
		if err != nil {
			logrus.Error(err)
			continue
		}

		if _, ok := versions[cc.Namespace]; !ok {
			versions[cc.Namespace] = []provv1.ManagedOSVersion{}
		}

		blockDel := false
		for _, v := range vers {
			vcpy := v.DeepCopy()
			vcpy.ObjectMeta.Namespace = cc.Namespace
			ownRef := *metav1.NewControllerRef(&cc, provv1.SchemeGroupVersion.WithKind("ManagedOSVersionChannel"))
			ownRef.BlockOwnerDeletion = &blockDel
			vcpy.ObjectMeta.OwnerReferences = []metav1.OwnerReference{ownRef}
			versions[cc.Namespace] = append(versions[cc.Namespace], *vcpy)
		}
	}

	// TODO: collect all errors
	for _, vv := range versions {
		for _, v := range vv {
			cli := c.OS.ManagedOSVersion()

			_, err := cli.Get(namespace, v.ObjectMeta.Name, metav1.GetOptions{})
			if err == nil {
				logrus.Debugf("there is already a version defined for %s(%s)", v.Name, v.Spec.Version)
				continue
			}
			_, err = cli.Create(&v)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
