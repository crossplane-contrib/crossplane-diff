/*
Copyright 2026 The Crossplane Authors.

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

package versioncmd

import (
	"context"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

const (
	errCreateK8sClientset        = "could not create the clientset for Kubernetes"
	errFetchCrossplaneDeployment = "could not fetch deployments"
)

// FetchCrossplaneVersion returns the Crossplane server version using the
// provided REST config. It mirrors the upstream
// github.com/crossplane/crossplane/v2/cmd/crank/version.FetchCrossplaneVersion
// but accepts a pre-built *rest.Config instead of calling ctrl.GetConfig, so
// callers can honor the user's kubeconfig context (e.g. --context) even when
// running inside a Kubernetes pod.
//
// TODO: Remove this fork once an upstream variant that accepts *rest.Config is
// available. Tracked in https://github.com/crossplane-contrib/crossplane-diff/issues/285.
func FetchCrossplaneVersion(ctx context.Context, cfg *rest.Config) (string, error) {
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", errors.Wrap(err, errCreateK8sClientset)
	}

	return fetchCrossplaneVersion(ctx, clientset)
}

// fetchCrossplaneVersion is the testable core. It takes a generic
// kubernetes.Interface so fake clientsets can drive unit tests.
func fetchCrossplaneVersion(ctx context.Context, clientset kubernetes.Interface) (string, error) {
	deployments, err := clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
		LabelSelector: "app=crossplane",
	})
	if err != nil {
		return "", errors.Wrap(err, errFetchCrossplaneDeployment)
	}

	for _, deployment := range deployments.Items {
		if v, ok := deployment.Labels["app.kubernetes.io/version"]; ok {
			if !strings.HasPrefix(v, "v") {
				v = "v" + v
			}

			return v, nil
		}

		if len(deployment.Spec.Template.Spec.Containers) > 0 {
			imageRef := deployment.Spec.Template.Spec.Containers[0].Image

			ref, err := name.ParseReference(imageRef)
			if err != nil {
				return "", errors.Wrap(err, "error parsing image reference")
			}

			if tagged, ok := ref.(name.Tag); ok {
				imageTag := tagged.TagStr()
				if !strings.HasPrefix(imageTag, "v") {
					imageTag = "v" + imageTag
				}

				return imageTag, nil
			}
		}
	}

	return "", errors.New("crossplane version or image tag not found")
}
