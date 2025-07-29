package main

import (
	"context"

	"k8s.io/client-go/rest"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/logging"

	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/core"
	xp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/crossplane"
	k8 "github.com/crossplane-contrib/crossplane-diff/cmd/diff/client/kubernetes"
)

// AppContext holds application-wide dependencies and clients.
type AppContext struct {
	K8sClients k8.Clients
	XpClients  xp.Clients
}

// NewAppContext creates a new AppContext with initialized clients.
func NewAppContext(config *rest.Config, logger logging.Logger) (*AppContext, error) {
	coreClients, err := core.NewClients(config)
	if err != nil {
		// error is already well-decorated
		return nil, err
	}

	tc := k8.NewTypeConverter(coreClients, logger)

	k8c := k8.Clients{
		Type:     tc,
		Apply:    k8.NewApplyClient(coreClients, tc, logger),
		Resource: k8.NewResourceClient(coreClients, tc, logger),
		Schema:   k8.NewSchemaClient(coreClients, tc, logger),
	}

	defClient := xp.NewDefinitionClient(k8c.Resource, logger)

	xpc := xp.Clients{
		Definition: defClient,
		Composition:  xp.NewCompositionClient(k8c.Resource, defClient, logger),
		Environment:  xp.NewEnvironmentClient(k8c.Resource, logger),
		Function:     xp.NewFunctionClient(k8c.Resource, logger),
		ResourceTree: xp.NewResourceTreeClient(coreClients.Tree, logger),
	}

	return &AppContext{
		K8sClients: k8c,
		XpClients:  xpc,
	}, nil
}

// Initialize initializes all clients.
func (a *AppContext) Initialize(ctx context.Context, logger logging.Logger) error {
	// Initialize Crossplane client
	if err := a.XpClients.Initialize(ctx, logger); err != nil {
		return errors.Wrap(err, "cannot initialize Crossplane client")
	}

	return nil
}
