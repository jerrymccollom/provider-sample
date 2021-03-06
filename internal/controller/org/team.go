/*
Copyright 2020 The Crossplane Authors.

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

package org

import (
	"context"
	"fmt"

	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	"github.com/google/go-github/v32/github"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"github.com/jerrymccollom/provider-sample/apis/org/v1alpha1"
	apisv1alpha1 "github.com/jerrymccollom/provider-sample/apis/v1alpha1"
	gitclient "github.com/jerrymccollom/provider-sample/internal/client"
)

const (
	errNotMyType    = "managed resource is not a MyType custom resource"
	errTrackPCUsage = "cannot track ProviderConfig usage"
	errGetPC        = "cannot get ProviderConfig"
	errNoSecretRef  = "ProviderConfig does not reference a credentials Secret"
	errGetSecret    = "cannot get credentials Secret"

	errNewClient = "cannot create new Service"
)

// Setup adds a controller that reconciles MyType managed resources.
func Setup(mgr ctrl.Manager, l logging.Logger, rl workqueue.RateLimiter) error {
	name := managed.ControllerName(v1alpha1.TeamGroupKind)

	o := controller.Options{
		RateLimiter: ratelimiter.NewDefaultManagedRateLimiter(rl),
	}

	r := managed.NewReconciler(mgr,
		resource.ManagedKind(v1alpha1.TeamGroupVersionKind),
		managed.WithExternalConnecter(&connector{
			kube:  mgr.GetClient(),
			usage: resource.NewProviderConfigUsageTracker(mgr.GetClient(), &apisv1alpha1.ProviderConfigUsage{})}),
		managed.WithLogger(l.WithValues("controller", name)),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))))

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o).
		For(&v1alpha1.Team{}).
		Complete(r)
}

// A connector is expected to produce an ExternalClient when its Connect method
// is called.
type connector struct {
	kube  client.Client
	usage resource.Tracker
}

// Connect typically produces an ExternalClient by:
// 1. Tracking that the managed resource is using a ProviderConfig.
// 2. Getting the managed resource's ProviderConfig.
// 3. Getting the ProviderConfig's credentials secret.
// 4. Using the credentials secret to form a client.
func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	cr, ok := mg.(*v1alpha1.Team)
	if !ok {
		return nil, errors.New(errNotMyType)
	}

	if err := c.usage.Track(ctx, mg); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	pc := &apisv1alpha1.ProviderConfig{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: cr.GetProviderConfigReference().Name}, pc); err != nil {
		return nil, errors.Wrap(err, errGetPC)
	}

	// A secret is the most common way to authenticate to a provider, but some
	// providers additionally support alternative authentication methods such as
	// IAM, so a reference is not required.
	ref := pc.Spec.Credentials.SecretRef
	if ref == nil {
		return nil, errors.New(errNoSecretRef)
	}

	s := &v1.Secret{}
	if err := c.kube.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, s); err != nil {
		return nil, errors.Wrap(err, errGetSecret)
	}

	svc, err := gitclient.NewClient(string(s.Data[ref.Key]))
	if err != nil {
		return nil, errors.Wrap(err, errNewClient)
	}

	return &external{service: svc}, nil
}

// An ExternalClient observes, then either creates, updates, or deletes an
// external resource to ensure it reflects the managed resource's desired state.
type external struct {
	// A 'client' used to connect to the external resource API. In practice this
	// would be something like an AWS SDK client.
	service *github.Client
}

func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.Team)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotMyType)
	}

	team, _, err := c.service.Teams.GetTeamBySlug(ctx, cr.Spec.ForProvider.Org, meta.GetExternalName(cr))
	if err != nil {
		return managed.ExternalObservation{
			ResourceExists: false,
		}, nil
	}

	if team.NodeID != nil {
		cr.Status.AtProvider.NodeID = *team.NodeID
	}

	upToDate := true
	if team != nil {
		if cr.Spec.ForProvider.Description != nil {
			if team.Description == nil || *team.Description != *cr.Spec.ForProvider.Description {
				upToDate = false
			}
		}
		if cr.Spec.ForProvider.Privacy != nil {
			if team.Privacy == nil || *team.Privacy != *cr.Spec.ForProvider.Privacy {
				upToDate = false
			}
		}
	}
	return managed.ExternalObservation{
		// Return false when the external resource does not exist. This lets
		// the managed resource reconciler know that it needs to call Create to
		// (re)create the resource, or that it has successfully been deleted.
		ResourceExists: true,

		// Return false when the external resource exists, but it not up to date
		// with the desired managed resource state. This lets the managed
		// resource reconciler know that it needs to call Update.
		ResourceUpToDate: upToDate,
	}, nil
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha1.Team)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotMyType)
	}

	fmt.Printf("Creating: %+v", cr)

	_, _, err := c.service.Teams.CreateTeam(ctx, cr.Spec.ForProvider.Org, github.NewTeam{
		Name:        meta.GetExternalName(cr),
		Description: cr.Spec.ForProvider.Description,
		Privacy:     cr.Spec.ForProvider.Privacy,
	})

	return managed.ExternalCreation{}, err
}

func (c *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	cr, ok := mg.(*v1alpha1.Team)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotMyType)
	}

	fmt.Printf("Updating: %+v", cr)

	_, _, err := c.service.Teams.EditTeamBySlug(ctx, cr.Spec.ForProvider.Org, meta.GetExternalName(cr), github.NewTeam{
		Name:        meta.GetExternalName(cr),
		Description: cr.Spec.ForProvider.Description,
		Privacy:     cr.Spec.ForProvider.Privacy,
	}, false)

	return managed.ExternalUpdate{}, err
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*v1alpha1.Team)
	if !ok {
		return errors.New(errNotMyType)
	}

	fmt.Printf("Deleting: %+v", cr)

	_, err := c.service.Teams.DeleteTeamBySlug(ctx, cr.Spec.ForProvider.Org, meta.GetExternalName(cr))

	return err
}
