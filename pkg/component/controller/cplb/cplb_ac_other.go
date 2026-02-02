//go:build !linux

// SPDX-FileCopyrightText: 2024 k0s authors
// SPDX-License-Identifier: Apache-2.0

package cplb

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	k0sAPI "github.com/k0sproject/k0s/pkg/apis/k0s/v1beta1"
	"github.com/k0sproject/k0s/pkg/config"
)

// Anycast doesn't work on non-Linux, so we cannot implement it at all.
// Just create the interface so that the CI doesn't complain.
type Anycast struct {
	K0sVars         *config.CfgVars
	Config          *k0sAPI.AnycastSpec
	DetailedLogging bool
	LogConfig       bool
	APIPort         int
	APIAddress      string
	KubeConfigPath  string
}

func (a *Anycast) Init(context.Context) error {
	return fmt.Errorf("%w: Anycast CPLB is not supported on %s", errors.ErrUnsupported, runtime.GOOS)
}

func (a *Anycast) Start(context.Context) error {
	return fmt.Errorf("%w: Anycast CPLB is not supported on %s", errors.ErrUnsupported, runtime.GOOS)
}

func (a *Anycast) Stop() error {
	return fmt.Errorf("%w: Anycast CPLB is not supported on %s", errors.ErrUnsupported, runtime.GOOS)
}
