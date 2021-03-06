// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package exec

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"sort"
	"sync"
	"time"

	"github.com/coreos/ignition/config"
	"github.com/coreos/ignition/src/exec/stages"
	"github.com/coreos/ignition/src/log"
	"github.com/coreos/ignition/src/providers"
)

const (
	DefaultFetchTimeout = time.Minute
)

var (
	ErrNoProviders = errors.New("no config providers were online")
	ErrTimeout     = errors.New("timed out while waiting for a config provider to come online")
)

type Engine struct {
	ConfigCache  string
	FetchTimeout time.Duration
	Logger       log.Logger
	Root         string
	providers    map[string]providers.Provider
}

func (e *Engine) AddProvider(provider providers.Provider) {
	if e.providers == nil {
		e.providers = map[string]providers.Provider{}
	}
	e.providers[provider.Name()] = provider
}

func (e Engine) Providers() []providers.Provider {
	keys := []string{}
	for key := range e.providers {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	providers := make([]providers.Provider, 0, len(e.providers))
	for _, key := range keys {
		providers = append(providers, e.providers[key])
	}
	return providers
}

func (e Engine) Run(stageName string) bool {
	config, err := e.acquireConfig()
	if err != nil {
		e.Logger.Crit("failed to acquire config: %v", err)
		return false
	}

	e.Logger.PushPrefix(stageName)
	defer e.Logger.PopPrefix()
	return stages.Get(stageName).Create(&e.Logger, e.Root).Run(config)
}

func (e Engine) acquireConfig() (cfg config.Config, err error) {
	// First try read the config @ e.ConfigCache.
	b, err := ioutil.ReadFile(e.ConfigCache)
	if err == nil {
		if err = json.Unmarshal(b, &cfg); err != nil {
			e.Logger.Crit("failed to parse cached config: %v", err)
		}
		return
	}

	// (Re)Fetch the config if the cache is unreadable.
	cfg, err = fetchConfig(e.Providers(), e.FetchTimeout)
	if err != nil {
		e.Logger.Crit("failed to fetch config: %v", err)
		return
	}
	e.Logger.Debug("fetched config: %+v", cfg)

	// Populate the config cache.
	b, err = json.Marshal(cfg)
	if err != nil {
		e.Logger.Crit("failed to marshal cached config: %v", err)
		return
	}
	if err = ioutil.WriteFile(e.ConfigCache, b, 0640); err != nil {
		e.Logger.Crit("failed to write cached config: %v", err)
		return
	}

	return
}

func fetchConfig(providers []providers.Provider, timeout time.Duration) (config.Config, error) {
	if provider, err := selectProvider(providers, timeout); err == nil {
		return provider.FetchConfig()
	} else {
		return config.Config{}, err
	}
}

func selectProvider(ps []providers.Provider, timeout time.Duration) (providers.Provider, error) {
	online := make(chan providers.Provider)
	wg := sync.WaitGroup{}
	stop := make(chan struct{})
	defer close(stop)

	for _, p := range ps {
		wg.Add(1)
		go func(provider providers.Provider) {
			defer wg.Done()

			for {
				if provider.IsOnline() {
					online <- provider
					return
				} else if !provider.ShouldRetry() {
					return
				}

				select {
				case <-time.After(provider.BackoffDuration()):
				case <-stop:
					return
				}
			}
		}(p)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	var provider providers.Provider
	select {
	case provider = <-online:
		return provider, nil
	case <-done:
		return nil, ErrNoProviders
	case <-time.After(timeout):
		return nil, ErrTimeout
	}
}
