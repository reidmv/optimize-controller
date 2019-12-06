/*
Copyright 2019 GramLabs, Inc.

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

package util

import (
	redskyclient "github.com/redskyops/k8s-experiment/redskyapi"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Red Sky server specific configuration flags

const (
	flagAddress = "address"
)

type ServerFlags struct {
	Address *string
}

func NewServerFlags() *ServerFlags {
	return &ServerFlags{
		Address: stringptr(""),
	}
}

func (f *ServerFlags) AddFlags(flags *pflag.FlagSet) {
	if f.Address != nil {
		flags.StringVar(f.Address, flagAddress, *f.Address, "Absolute URL of the Red Sky API.")
	}
}

func (f *ServerFlags) ToClientConfig() (*viper.Viper, error) {
	clientConfig, err := redskyclient.DefaultConfig()
	if err != nil {
		return nil, err
	}

	if f.Address != nil && *f.Address != "" {
		// TODO How do we use Viper pflags integration with this code?
		clientConfig.Set("address", *f.Address)
	}

	return clientConfig, nil
}
