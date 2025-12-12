/*
Copyright 2025 The Kubernetes Authors.

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

// examples/sharded-kind/main.go
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sync/errgroup"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/manager/coordinator/sharded"
	"sigs.k8s.io/multicluster-runtime/pkg/manager/coordinator/sharded/peers"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
	"sigs.k8s.io/multicluster-runtime/providers/kind"
)

func main() {
	klog.Background()
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("sharded-kind")

	ctx := ctrl.SetupSignalHandler()
	if err := run(ctx); err != nil {
		log.Error(err, "exiting")
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("get kubeconfig: %w", err)
	}

	// Host cluster: used for peer + shard Leases (coordination).
	host, err := cluster.New(cfg)
	if err != nil {
		return fmt.Errorf("create host cluster: %w", err)
	}
	kubeClient, err := client.New(cfg, client.Options{Scheme: host.GetScheme()})
	if err != nil {
		return fmt.Errorf("create kube client: %w", err)
	}

	// Kind provider: discovers local kind clusters (optionally filtered by prefix).
	kindPrefix := os.Getenv("KIND_PREFIX")
	provider := kind.New(kind.Options{Prefix: kindPrefix})

	// Optional explicit peer identity so multiple local processes can co-exist.
	selfID := os.Getenv("PEER_ID") // defaulted by registry to os.Hostname() if empty
	reg := peers.NewLeaseRegistry(kubeClient, "kube-system", "mcr-peer", selfID, 1, ctrl.Log.WithName("peers"))

	// Multicluster manager with sharded coordinator.
	mgr, err := mcmanager.New(cfg, provider, manager.Options{
		// Disable metrics listener to allow multiple local peers on one machine.
		Metrics: server.Options{BindAddress: "0"},
	},
		mcmanager.WithCoordinator(
			sharded.New(kubeClient, ctrl.Log.WithName("sharded-coordinator"),
				sharded.WithShardLease("kube-system", "mcr-shard"),
				sharded.WithPerClusterLease(true),
				sharded.WithPeerRegistry(reg),
			),
		),
	)
	if err != nil {
		return fmt.Errorf("create multicluster manager: %w", err)
	}

	// Simple controller that logs ConfigMaps for whichever clusters this peer owns.
	if err := mcbuilder.ControllerManagedBy(mgr).
		Named("multicluster-configmaps").
		For(&corev1.ConfigMap{}).
		Complete(mcreconcile.Func(func(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
			l := ctrl.LoggerFrom(ctx).WithValues("cluster", req.ClusterName)

			cl, err := mgr.GetCluster(ctx, req.ClusterName)
			if err != nil {
				return ctrl.Result{}, err
			}

			cm := &corev1.ConfigMap{}
			if err := cl.GetClient().Get(ctx, req.Request.NamespacedName, cm); err != nil {
				if apierrors.IsNotFound(err) {
					return ctrl.Result{}, nil
				}
				return ctrl.Result{}, err
			}

			l.Info("Reconciling ConfigMap", "ns", cm.GetNamespace(), "name", cm.GetName())
			if host, _ := os.Hostname(); host != "" {
				l.Info("Handled by peer", "peer", host)
			}
			return ctrl.Result{}, nil
		})); err != nil {
		return fmt.Errorf("build controller: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return ignoreCanceled(host.Start(ctx)) })
	g.Go(func() error { return ignoreCanceled(mgr.Start(ctx)) })
	return g.Wait()
}

func ignoreCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
