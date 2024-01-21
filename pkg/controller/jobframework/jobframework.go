package jobframework

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"sigs.k8s.io/kueue/pkg/constants"
	"sigs.k8s.io/kueue/pkg/controller/jobs/noop"
	"sigs.k8s.io/kueue/pkg/util/cert"
	"sigs.k8s.io/kueue/pkg/util/kubeversion"
)

func SetupControllers(
	mgr ctrl.Manager,
	setupLog logr.Logger,
	kubeVersionFunc func(fwkName string, serverVersionFetcher *kubeversion.ServerVersionFetcher, opts ...Option) error,
	certsReady chan struct{},
	opts ...Option,
) error {
	// The controllers won't work until the webhooks are operating, and the webhook won't work until the
	// certs are all in place.
	cert.WaitForCertsReady(setupLog, certsReady)

	options := DefaultOptions
	for _, opt := range opts {
		opt(&options)
	}

	return ForEachIntegration(func(name string, cb IntegrationCallbacks) error {
		log := setupLog.WithValues("jobFrameworkName", name)
		fwkNamePrefix := fmt.Sprintf("jobFrameworkName %q", name)

		if options.EnabledFrameworks.Has(name) {
			gvk, err := apiutil.GVKForObject(cb.JobType, mgr.GetScheme())
			if err != nil {
				return fmt.Errorf("%s: %w", fwkNamePrefix, err)
			}
			if _, err = mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
				if !meta.IsNoMatchError(err) {
					return fmt.Errorf("%s: %w", fwkNamePrefix, err)
				}
				log.Info("No matching API in the server for job framework, skipped setup of controller and webhook")
			} else {
				if err = cb.NewReconciler(
					mgr.GetClient(),
					mgr.GetEventRecorderFor(fmt.Sprintf("%s-%s-controller", name, constants.KueueName)),
					opts...,
				).SetupWithManager(mgr); err != nil {
					return fmt.Errorf("%s: %w", fwkNamePrefix, err)
				}
				if kubeVersionFunc != nil {
					if err = kubeVersionFunc(name, options.KubeServerVersion, opts...); err != nil {
						return fmt.Errorf("%s: failed to configure reconcilers: %w", fwkNamePrefix, err)
					}
				}
				if err = cb.SetupWebhook(mgr, opts...); err != nil {
					return fmt.Errorf("%s: unable to create webhook: %w", fwkNamePrefix, err)
				}
				log.Info("Set up controller and webhook for job framework")
				return nil
			}
		}
		if err := noop.SetupWebhook(mgr, cb.JobType); err != nil {
			return fmt.Errorf("%s: unable to create noop webhook: %w", fwkNamePrefix, err)
		}
		return nil
	})
}

func SetupIndexes(ctx context.Context, mgr ctrl.Manager, opts ...Option) error {
	options := DefaultOptions
	for _, opt := range opts {
		opt(&options)
	}
	return ForEachIntegration(func(name string, cb IntegrationCallbacks) error {
		if options.EnabledFrameworks.Has(name) {
			if err := cb.SetupIndexes(ctx, mgr.GetFieldIndexer()); err != nil {
				return fmt.Errorf("jobFrameworkName %q: %w", name, err)
			}
		}
		return nil
	})
}
