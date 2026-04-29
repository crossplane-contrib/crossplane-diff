/*
Copyright 2026 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package versioncmd

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func deployment(name, imageTag string, labels map[string]string) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "crossplane-system",
			Labels:    map[string]string{"app": "crossplane"},
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "crossplane",
							Image: "crossplane/crossplane:" + imageTag,
						},
					},
				},
			},
		},
	}
	maps.Copy(d.Labels, labels)

	return d
}

func TestFetchCrossplaneVersion_VersionLabel(t *testing.T) {
	tests := map[string]struct {
		labelValue string
		want       string
	}{
		"PlainVersionGetsVPrefix": {
			labelValue: "2.0.2",
			want:       "v2.0.2",
		},
		"VPrefixedVersionUnchanged": {
			labelValue: "v2.0.2",
			want:       "v2.0.2",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			clientset := fake.NewClientset(
				deployment("crossplane", "v2.0.0", map[string]string{
					"app.kubernetes.io/version": tc.labelValue,
				}),
			)

			got, err := fetchCrossplaneVersion(context.Background(), clientset)
			if err != nil {
				t.Fatalf("fetchCrossplaneVersion: %v", err)
			}

			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFetchCrossplaneVersion_FallBackToImageTag(t *testing.T) {
	// No version label — must use container image tag.
	clientset := fake.NewClientset(
		deployment("crossplane", "v2.1.0", nil),
	)

	got, err := fetchCrossplaneVersion(context.Background(), clientset)
	if err != nil {
		t.Fatalf("fetchCrossplaneVersion: %v", err)
	}

	if got != "v2.1.0" {
		t.Errorf("got %q, want v2.1.0", got)
	}
}

func TestFetchCrossplaneVersion_ImageTagWithoutVPrefixGetsV(t *testing.T) {
	clientset := fake.NewClientset(
		deployment("crossplane", "2.1.0", nil),
	)

	got, err := fetchCrossplaneVersion(context.Background(), clientset)
	if err != nil {
		t.Fatalf("fetchCrossplaneVersion: %v", err)
	}

	if got != "v2.1.0" {
		t.Errorf("got %q, want v2.1.0", got)
	}
}

func TestFetchCrossplaneVersion_NoDeployments(t *testing.T) {
	clientset := fake.NewClientset()

	_, err := fetchCrossplaneVersion(context.Background(), clientset)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "crossplane version or image tag not found") {
		t.Errorf("error = %v, want the specific no-deployments message", err)
	}
}

func TestFetchCrossplaneVersion_ListError(t *testing.T) {
	clientset := fake.NewClientset()
	clientset.PrependReactor("list", "deployments", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			appsv1.Resource("deployments"), "", errors.New("nope"))
	})

	_, err := fetchCrossplaneVersion(context.Background(), clientset)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "could not fetch deployments") {
		t.Errorf("error = %v, want wrapped 'could not fetch deployments'", err)
	}
}
