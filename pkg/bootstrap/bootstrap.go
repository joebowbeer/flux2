/*
Copyright 2021 The Flux authors

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

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1beta1"
	"github.com/fluxcd/pkg/apis/meta"

	"github.com/fluxcd/flux2/pkg/manifestgen/install"
	"github.com/fluxcd/flux2/pkg/manifestgen/sourcesecret"
	"github.com/fluxcd/flux2/pkg/manifestgen/sync"
)

var ErrReconciledWithWarning = errors.New("reconciled with warning")

type Reconciler interface {
	// ReconcileComponents reconciles the components by generating the
	// manifests with the provided values, committing them to Git and
	// pushing to remote if there are any changes, and applying them
	// to the cluster.
	ReconcileComponents(ctx context.Context, manifestsBase string, options install.Options) error

	// ReconcileSourceSecret reconciles the source secret by generating
	// a new secret with the provided values if the secret does not
	// already exists on the cluster, or if any of the configuration
	// options changed. Before the secret is applied to the cluster
	// the PostGenerateSecretCallback is called, any errors returned
	// by this callback will abort the reconciliation process.
	ReconcileSourceSecret(ctx context.Context, options sourcesecret.Options, postGenerate PostGenerateSecretCallback) error

	// ReconcileSyncConfig
	ReconcileSyncConfig(ctx context.Context, options sync.Options, pollInterval, timeout time.Duration) error
}

type RepositoryReconciler interface {
	ReconcileRepository(ctx context.Context) error
}

type PostGenerateSecretCallback func(ctx context.Context, secret corev1.Secret) error

func Run(ctx context.Context, reconciler Reconciler, manifestsBase string,
	installOpts install.Options, secretOpts sourcesecret.Options, syncOpts sync.Options,
	pollInterval, timeout time.Duration) error {

	var err error
	if r, ok := reconciler.(RepositoryReconciler); ok {
		if err = r.ReconcileRepository(ctx); err != nil && !errors.Is(err, ErrReconciledWithWarning) {
			return err
		}
	}

	if err := reconciler.ReconcileComponents(ctx, manifestsBase, installOpts); err != nil {
		return err
	}

	if err := reconciler.ReconcileSourceSecret(ctx, secretOpts, nil); err != nil {
		return err
	}
	if err := reconciler.ReconcileSyncConfig(ctx, syncOpts, pollInterval, timeout); err != nil {
		return err
	}

	return err
}

func shouldInstallManifests(ctx context.Context, kube client.Client, namespace string) bool {
	namespacedName := types.NamespacedName{
		Namespace: namespace,
		Name:      namespace,
	}
	var k kustomizev1.Kustomization
	if err := kube.Get(ctx, namespacedName, &k); err != nil {
		return true
	}
	return k.Status.LastAppliedRevision == ""
}

func secretExists(ctx context.Context, kube client.Client, objKey client.ObjectKey) (bool, error) {
	if err := kube.Get(ctx, objKey, &corev1.Secret{}); err != nil {
		if apierr.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func reconcileSecret(ctx context.Context, kube client.Client, secret corev1.Secret) error {
	objKey := client.ObjectKeyFromObject(&secret)
	var existing corev1.Secret
	err := kube.Get(ctx, objKey, &existing)
	if err != nil {
		if apierr.IsNotFound(err) {
			return kube.Create(ctx, &secret)
		}
		return err
	}
	existing.StringData = secret.StringData
	return kube.Update(ctx, &existing)
}

func kustomizationReconciled(ctx context.Context, kube client.Client, objKey client.ObjectKey,
	kustomization *kustomizev1.Kustomization, expectRevision string) func() (bool, error) {

	return func() (bool, error) {
		if err := kube.Get(ctx, objKey, kustomization); err != nil {
			return false, err
		}

		// Confirm the state we are observing is for the current generation
		if kustomization.Generation != kustomization.Status.ObservedGeneration {
			return false, nil
		}

		// Confirm the given revision has been attempted by the controller
		if kustomization.Status.LastAttemptedRevision != expectRevision {
			return false, nil
		}

		// Confirm the resource is healthy
		if c := apimeta.FindStatusCondition(kustomization.Status.Conditions, meta.ReadyCondition); c != nil {
			switch c.Status {
			case metav1.ConditionTrue:
				return true, nil
			case metav1.ConditionFalse:
				return false, fmt.Errorf(c.Message)
			}
		}
		return false, nil
	}
}
