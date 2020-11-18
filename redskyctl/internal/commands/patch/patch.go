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

package patch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"path/filepath"
	"strconv"
	"strings"

	app "github.com/redskyops/redskyops-controller/api/apps/v1alpha1"
	redsky "github.com/redskyops/redskyops-controller/api/v1beta1"
	"github.com/redskyops/redskyops-controller/internal/experiment"
	"github.com/redskyops/redskyops-controller/internal/patch"
	"github.com/redskyops/redskyops-controller/internal/server"
	"github.com/redskyops/redskyops-controller/internal/template"
	"github.com/redskyops/redskyops-controller/redskyctl/internal/commander"
	experimentctl "github.com/redskyops/redskyops-controller/redskyctl/internal/commands/generate/experiment"
	"github.com/redskyops/redskyops-controller/redskyctl/internal/kustomize"
	"github.com/redskyops/redskyops-go/pkg/config"
	experimentsapi "github.com/redskyops/redskyops-go/pkg/redskyapi/experiments/v1alpha1"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/kustomize/api/filesys"
	"sigs.k8s.io/kustomize/api/resid"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/kio"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// Options are the configuration options for creating a patched experiment
type Options struct {
	// Config is the Red Sky Configuration used to generate the controller installation
	Config *config.RedSkyConfig
	// ExperimentsAPI is used to interact with the Red Sky Experiments API
	ExperimentsAPI experimentsapi.API
	// IOStreams are used to access the standard process streams
	commander.IOStreams

	inputFiles  []string
	trialNumber int
	trialName   string

	// This is used for testing
	Fs          filesys.FileSystem
	inputData   []byte
	experiment  *redsky.Experiment
	application *app.Application
	resources   map[string]struct{}
}

// NewCommand creates a command for performing a patch
func NewCommand(o *Options) *cobra.Command {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Llongfile)

	cmd := &cobra.Command{
		Use:   "patch",
		Short: "Create a patched manifest using trial parameters",
		Long:  "Create a patched manifest using the parameters from the specified trial",

		PreRunE: func(cmd *cobra.Command, args []string) error {
			commander.SetStreams(&o.IOStreams, cmd)

			var err error
			if o.ExperimentsAPI == nil {
				err = commander.SetExperimentsAPI(&o.ExperimentsAPI, o.Config, cmd)
			}

			return err
		},
		RunE: commander.WithContextE(o.runner),
	}

	cmd.Flags().StringSliceVar(&o.inputFiles, "file", []string{""}, "experiment and related manifests to patch, - for stdin")
	cmd.Flags().IntVar(&o.trialNumber, "trialnumber", -1, "trial number")
	cmd.Flags().StringVar(&o.trialName, "trialname", "", "trial name")

	return cmd
}

func (o *Options) readInput() error {
	// Do an in memory filesystem so we can properly handle stdin
	if o.Fs == nil {
		o.Fs = filesys.MakeFsInMemory()
	}

	if o.resources == nil {
		o.resources = make(map[string]struct{})
	}

	kioInputs := []kio.Reader{}

	for _, filename := range o.inputFiles {
		r, err := o.IOStreams.OpenFile(filename)
		if err != nil {
			return err
		}
		defer r.Close()

		data, err := ioutil.ReadAll(r)
		if err != nil {
			return err
		}

		if filename == "-" {
			filename = "stdin.yaml"
		}

		if err := o.Fs.WriteFile(filepath.Base(filename), data); err != nil {
			return err
		}

		kioInputs = append(kioInputs, &kio.ByteReader{Reader: bytes.NewReader(data)})

		// Track all of the input files so we can use them as kustomize resources later on
		o.resources[filepath.Base(filename)] = struct{}{}
	}

	var inputsBuf bytes.Buffer

	// Aggregate all inputs
	allInput := kio.Pipeline{
		Inputs:  kioInputs,
		Outputs: []kio.Writer{kio.ByteWriter{Writer: &inputsBuf}},
	}
	if err := allInput.Execute(); err != nil {
		return err
	}

	o.inputData = inputsBuf.Bytes()

	return nil
}

func (o *Options) extractApplication() error {
	var appBuf bytes.Buffer

	// Render Experiment
	appInput := kio.Pipeline{
		Inputs:  []kio.Reader{&kio.ByteReader{Reader: bytes.NewReader(o.inputData)}},
		Filters: []kio.Filter{kio.FilterFunc(filter("Application"))},
		Outputs: []kio.Writer{kio.ByteWriter{Writer: &appBuf}},
	}
	if err := appInput.Execute(); err != nil {
		return err
	}

	// We don't want to bail if we cant find an application since we'll handle this later
	if appBuf.Len() == 0 {
		return nil
	}

	o.application = &app.Application{}

	return commander.NewResourceReader().ReadInto(ioutil.NopCloser(&appBuf), o.application)
}

func (o *Options) extractExperiment() error {
	var experimentBuf bytes.Buffer

	// Render Experiment
	experimentInput := kio.Pipeline{
		Inputs:  []kio.Reader{&kio.ByteReader{Reader: bytes.NewReader(o.inputData)}},
		Filters: []kio.Filter{kio.FilterFunc(filter("Experiment"))},
		Outputs: []kio.Writer{kio.ByteWriter{Writer: &experimentBuf}},
	}
	if err := experimentInput.Execute(); err != nil {
		return err
	}

	// We don't want to bail if we cant find an experiment since we'll handle this later
	if experimentBuf.Len() == 0 {
		return nil
	}

	o.experiment = &redsky.Experiment{}

	return commander.NewResourceReader().ReadInto(ioutil.NopCloser(&experimentBuf), o.experiment)
}

// filter returns a filter function to exctract a specified `kind` from the input.
func filter(kind string) kio.FilterFunc {
	return func(input []*yaml.RNode) ([]*yaml.RNode, error) {
		var output kio.ResourceNodeSlice
		for i := range input {
			m, err := input[i].GetMeta()
			if err != nil {
				return nil, err
			}
			if m.Kind != kind {
				continue
			}
			output = append(output, input[i])
		}
		return output, nil
	}
}

func (o *Options) runner(ctx context.Context) error {
	if o.trialName == "" {
		return fmt.Errorf("a trial name must be specified")
	}

	if err := o.readInput(); err != nil {
		return err
	}

	// See if we have been given an applcation
	if err := o.extractApplication(); err != nil {
		return fmt.Errorf("got an error when looking for application: %w", err)
	}

	// See if we have been given an experiment
	if err := o.extractExperiment(); err != nil {
		return fmt.Errorf("got an error when looking for experiment: %w", err)
	}

	switch {
	case o.application != nil:
		// Reset/Restrict application resources to only those specified by the application
		// and resources generated by the generator
		o.resources = make(map[string]struct{})
		for _, file := range o.application.Resources {
			o.resources[file] = struct{}{}
		}

		if err := o.generateExperiment(); err != nil {
			return err
		}
	case o.experiment != nil:
		// Dont need to do anything special
	default:
		return fmt.Errorf("unable to identify an experiment or application")
	}

	// At this point we must have an experiment
	if o.experiment == nil {
		return fmt.Errorf("unable to find an experiment")
	}

	// look up trial from api
	trialItem, err := o.getTrialByID(ctx, o.experiment.Name)
	if err != nil {
		return err
	}

	trial := &redsky.Trial{}
	experiment.PopulateTrialFromTemplate(o.experiment, trial)
	server.ToClusterTrial(trial, &trialItem.TrialAssignments)

	// render patches
	var patches map[string]types.Patch
	patches, err = createKustomizePatches(o.experiment.Spec.Patches, trial)
	if err != nil {
		return err
	}

	resourceNames := make([]string, 0, len(o.resources))
	for name := range o.resources {
		resourceNames = append(resourceNames, name)

	}

	yamls, err := kustomize.Yamls(
		kustomize.WithFS(o.Fs),
		kustomize.WithResourceNames(resourceNames),
		kustomize.WithPatches(patches),
	)
	if err != nil {
		return err
	}

	fmt.Fprintln(o.Out, string(yamls))

	return nil
}

func (o *Options) generateExperiment() error {
	gen := experimentctl.NewGenerator(o.Fs)
	gen.Application = *o.application
	gen.ContainerResourcesSelectors = experimentctl.DefaultContainerResourcesSelectors()

	if gen.Application.Parameters != nil && gen.Application.Parameters.ContainerResources != nil {
		ls := labels.Set(gen.Application.Parameters.ContainerResources.Labels).String()

		for i := range gen.ContainerResourcesSelectors {
			gen.ContainerResourcesSelectors[i].LabelSelector = ls
		}
	}

	list, err := gen.Generate()
	if err != nil {
		return err
	}

	for idx, listItem := range list.Items {
		listBytes, err := listItem.Marshal()
		if err != nil {
			return err
		}

		assetName := fmt.Sprintf("%s%d%s", "application-assets", idx, ".yaml")
		if err := o.Fs.WriteFile(assetName, listBytes); err != nil {
			return err
		}

		o.resources[assetName] = struct{}{}

		if te, ok := list.Items[idx].Object.(*redsky.Experiment); ok {
			o.experiment = &redsky.Experiment{}
			te.DeepCopyInto(o.experiment)
			break
		}
	}

	return nil
}

func (o *Options) getTrialByID(ctx context.Context, experimentName string) (*experimentsapi.TrialItem, error) {
	query := &experimentsapi.TrialListQuery{
		Status: []experimentsapi.TrialStatus{experimentsapi.TrialCompleted},
	}

	trialList, err := o.getTrials(ctx, experimentName, query)
	if err != nil {
		return nil, err
	}

	// Cut off just the trial number from the trial name
	trialNum := o.trialName[strings.LastIndex(o.trialName, "-")+1:]
	trialNumber, err := strconv.Atoi(trialNum)
	if err != nil {
		return nil, err
	}

	// Isolate the given trial we want by number
	var wantedTrial *experimentsapi.TrialItem
	for _, trial := range trialList.Trials {
		if trial.Number == int64(trialNumber) {
			wantedTrial = &trial
			break
		}
	}

	if wantedTrial == nil {
		return nil, fmt.Errorf("trial not found")
	}

	return wantedTrial, nil
}

// getTrials gets all trials from the redsky api for a given experiment.
func (o *Options) getTrials(ctx context.Context, experimentName string, query *experimentsapi.TrialListQuery) (trialList experimentsapi.TrialList, err error) {
	if o.ExperimentsAPI == nil {
		return trialList, fmt.Errorf("unable to connect to api server")
	}

	experiment, err := o.ExperimentsAPI.GetExperimentByName(ctx, experimentsapi.NewExperimentName(experimentName))
	if err != nil {
		return trialList, err
	}

	if experiment.TrialsURL == "" {
		return trialList, fmt.Errorf("unable to identify trial")
	}

	return o.ExperimentsAPI.GetAllTrials(ctx, experiment.TrialsURL, query)
}

// createKustomizePatches translates a patchTemplate into a kustomize (json) patch
func createKustomizePatches(patchSpec []redsky.PatchTemplate, trial *redsky.Trial) (map[string]types.Patch, error) {
	te := template.New()
	patches := map[string]types.Patch{}

	for idx, expPatch := range patchSpec {
		ref, data, err := patch.RenderTemplate(te, trial, &expPatch)
		if err != nil {
			return nil, err
		}

		// Surely there's got to be a better way
		// // Transition patch from json to map[string]interface
		m := make(map[string]interface{})
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}

		u := &unstructured.Unstructured{}
		// // Set patch data first ( otherwise it overwrites everything else )
		u.SetUnstructuredContent(m)
		// // Define object/type meta
		u.SetName(ref.Name)
		u.SetNamespace(ref.Namespace)
		u.SetGroupVersionKind(ref.GroupVersionKind())
		// // Profit
		b, err := u.MarshalJSON()
		if err != nil {
			return nil, err
		}

		patches[fmt.Sprintf("%s-%d", "patch", idx)] = types.Patch{
			Patch: string(b),
			Target: &types.Selector{
				Gvk: resid.Gvk{
					Group:   ref.GroupVersionKind().Group,
					Version: ref.GroupVersionKind().Version,
					Kind:    ref.GroupVersionKind().Kind,
				},
				Name:      ref.Name,
				Namespace: ref.Namespace,
			},
		}
	}

	return patches, nil
}
