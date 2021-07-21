/*
Copyright 2020 GramLabs, Inc.

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

package experiment

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/go-logr/logr"
	redskyappsv1alpha1 "github.com/thestormforge/optimize-controller/v2/api/apps/v1alpha1"
	redskyv1beta2 "github.com/thestormforge/optimize-controller/v2/api/v1beta2"
	"github.com/thestormforge/optimize-controller/v2/internal/server"
	"github.com/thestormforge/optimize-go/pkg/api"
	applications "github.com/thestormforge/optimize-go/pkg/api/applications/v2"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

type Runner struct {
	client        client.Client
	apiClient     applications.API
	log           logr.Logger
	kubectlExecFn func(cmd *exec.Cmd) ([]byte, error)
	errCh         chan (error)
}

func New(kclient client.Client, logger logr.Logger) (*Runner, error) {
	api, err := server.NewApplicationAPI(context.Background(), "TODO - user agent")
	if err != nil {
		return nil, err
	}

	return &Runner{
		client:    kclient,
		apiClient: api,
		log:       logger,
		errCh:     make(chan error),
	}, nil
}

// This doesnt necessarily need to live here, but seemed to make sense
func (r *Runner) Run(ctx context.Context) {
	go r.handleErrors(ctx)

	// TODO
	query := applications.ActivityFeedQuery{}
	query.SetType("poll", applications.TagScan, applications.TagRun)
	subscriber, err := r.apiClient.SubscribeActivity(ctx, query)
	if err != nil {
		// This should be a hard error; is panic too hard?
		panic(fmt.Sprintf("unable to query application activity %s", err))
	}

	activityCh := make(chan applications.ActivityItem)
	go subscriber.Subscribe(ctx, activityCh)

	for {
		select {
		case <-ctx.Done():
			return
		case activity := <-activityCh:
			// TODO might want to consider moving this to a func so we can defer delete activity and maybe
			// revamp the errCh nonsense

			// Ensure we actually have an action to perform
			if len(activity.Tags) != 1 {
				r.errCh <- fmt.Errorf("%s %d", "invalid number of activity tags, expected 1 got", len(activity.Tags))
				continue
			}

			activityCtx, _ := context.WithCancel(ctx)

			// Activity feed provides us with a scenario URL
			scenario, err := r.apiClient.GetScenario(activityCtx, activity.URL)
			if err != nil {
				// TODO enrich this later
				r.errCh <- err
				continue
			}

			// Need to fetch top level application so we can get the resources
			applicationURL := scenario.Link(api.RelationUp)
			if applicationURL == "" {
				r.errCh <- fmt.Errorf("no matching application URL for scenario")
			}

			templateURL := scenario.Link(api.RelationTemplate)
			if templateURL == "" {
				r.errCh <- fmt.Errorf("no matching template URL for scenario")
			}

			apiApp, err := r.apiClient.GetApplication(activityCtx, applicationURL)
			if err != nil {
				r.errCh <- fmt.Errorf("%s (%s): %w", "unable to get application", activity.URL, err)
				continue
			}

			var assembledApp *redskyappsv1alpha1.Application
			if assembledApp, err = r.scan(apiApp, scenario); err != nil {
				r.errCh <- err
				continue
			}

			assembledBytes, err := r.generateApp(*assembledApp)
			if err != nil {
				r.errCh <- err
				continue
			}

			exp := &redskyv1beta2.Experiment{}
			if err := yaml.Unmarshal(assembledBytes, exp); err != nil {
				r.errCh <- fmt.Errorf("%s: %w", "invalid experiment generated", err)
				continue
			}

			switch activity.Tags[0] {
			case applications.TagScan:
				template, err := server.ClusterExperimentToAPITemplate(exp)
				if err != nil {
					r.errCh <- err
					continue
				}

				if err := r.apiClient.UpdateTemplate(ctx, templateURL, *template); err != nil {
					r.errCh <- err
					continue
				}
			case applications.TagRun:
				// We wont compare existing scan with current scan
				// so we can preserve changes via UI

				// Get previous template
				previousTemplate, err := r.apiClient.GetTemplate(ctx, templateURL)
				if err != nil {
					r.errCh <- err
					continue
				}

				// Overwrite current scan results with previous scan results
				if err = server.APITemplateToClusterExperiment(exp, &previousTemplate); err != nil {
					r.errCh <- err
					continue
				}

				// At this point the experiment should be good to create/deploy/run
				// so let's create all the resources and #profit

				// Create additional RBAC ( primarily for setup task )
				r.createServiceAccount(ctx, assembledBytes)

				r.createClusterRole(ctx, assembledBytes)

				r.createClusterRoleBinding(ctx, assembledBytes)

				// Create configmap for load test
				r.createConfigMap(ctx, assembledBytes)

				r.createExperiment(ctx, exp)
			}

			// if err := r.apiClient.UpdateActivity(ctx, activity.URL, ?); err != nil {
			//   r.errCh <- err
			//   continue
			// }

			// if err := r.apiClient.DeleteActivity(ctx, activity.URL); err != nil {
			// 	r.errCh <- err
			// 	continue
			// }
		}
	}
}

func (r *Runner) handleErrors(ctx context.Context) {
	/*
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-r.errCh:
				r.log.Error(err, "failed to generate experiment from application")

				// TODO how do we want to pass through this additional info
				// Should we create a new error type ( akin to capture error ) with this additional metadata

				if err := r.apiClient.UpdateApplicationActivity(ctx, "activity url", applications.Activity{}); err != nil {
					continue
				}

				if err := r.apiClient.DeleteActivity(ctx, "activity url"); err != nil {
					continue
				}
			}
		}
	*/
}

func (r *Runner) createServiceAccount(ctx context.Context, data []byte) {
	serviceAccount := &corev1.ServiceAccount{}
	if err := yaml.Unmarshal(data, serviceAccount); err != nil {
		r.errCh <- fmt.Errorf("%s: %w", "invalid service account", err)
		return
	}

	// Only create the service account if it does not exist
	existingServiceAccount := &corev1.ServiceAccount{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: serviceAccount.Name, Namespace: serviceAccount.Namespace}, existingServiceAccount); err != nil {
		if err := r.client.Create(ctx, serviceAccount); err != nil {
			r.errCh <- fmt.Errorf("%s: %w", "failed to create service account", err)
		}
	}
}

func (r *Runner) createClusterRole(ctx context.Context, data []byte) {
	clusterRole := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(data, clusterRole); err != nil {
		r.errCh <- fmt.Errorf("%s: %w", "invalid cluster role", err)
		return
	}

	// Only create the service account if it does not exist
	existingClusterRole := &rbacv1.ClusterRole{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: clusterRole.Name, Namespace: clusterRole.Namespace}, existingClusterRole); err != nil {
		if err := r.client.Create(ctx, clusterRole); err != nil {
			r.errCh <- fmt.Errorf("%s: %w", "failed to create clusterRole", err)
		}
	}
}

func (r *Runner) createClusterRoleBinding(ctx context.Context, data []byte) {
	clusterRoleBinding := &rbacv1.ClusterRoleBinding{}
	if err := yaml.Unmarshal(data, clusterRoleBinding); err != nil {
		r.errCh <- fmt.Errorf("%s: %w", "invalid cluster role binding", err)
		return
	}

	existingClusterRoleBinding := &rbacv1.ClusterRoleBinding{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: clusterRoleBinding.Name, Namespace: clusterRoleBinding.Namespace}, existingClusterRoleBinding); err != nil {
		if err := r.client.Create(ctx, clusterRoleBinding); err != nil {
			r.errCh <- fmt.Errorf("%s: %w", "failed to create cluster role binding", err)
		}
	}
}

func (r *Runner) createConfigMap(ctx context.Context, data []byte) {
	configMap := &corev1.ConfigMap{}
	if err := yaml.Unmarshal(data, configMap); err != nil {
		r.errCh <- fmt.Errorf("%s: %w", "invalid config map", err)
		return
	}

	existingConfigMap := &corev1.ConfigMap{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: configMap.Name, Namespace: configMap.Namespace}, existingConfigMap); err != nil {
		if err := r.client.Create(ctx, configMap); err != nil {
			r.errCh <- fmt.Errorf("%s: %w", "failed to create config map", err)
		}
	} else {
		if err := r.client.Update(ctx, configMap); err != nil {
			r.errCh <- fmt.Errorf("%s: %w", "failed to update config map", err)
		}
	}
}

func (r *Runner) createExperiment(ctx context.Context, exp *redskyv1beta2.Experiment) {
	existingExperiment := &redskyv1beta2.Experiment{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: exp.Name, Namespace: exp.Namespace}, existingExperiment); err != nil {
		if err := r.client.Create(ctx, exp); err != nil {
			// api.UpdateStatus("failed")
			r.errCh <- fmt.Errorf("%s: %w", "unable to create experiment in cluster", err)
		}
	} else {
		// Update the experiment ( primarily to set replicas from 0 -> 1 )
		if err := r.client.Update(ctx, exp); err != nil {
			// api.UpdateStatus("failed")
			r.errCh <- fmt.Errorf("%s: %w", "unable to start experiment", err)
		}
	}
}
