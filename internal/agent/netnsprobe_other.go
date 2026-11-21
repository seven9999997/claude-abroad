//go:build !linux

package agent

import (
	"context"
	"errors"

	"claude-abroad/internal/kmod"
)

var errProbeLinuxOnly = errors.New("探查只能在虚拟机（Linux）里运行")

type otherProbeOps struct{}

func newProbeOps(func(names []string) []kmod.Result) probeOps { return otherProbeOps{} }

func (otherProbeOps) Run(context.Context, ...string) (string, error)   { return "", errProbeLinuxOnly }
func (otherProbeOps) Start(context.Context, ...string) (func(), error) { return nil, errProbeLinuxOnly }
func (otherProbeOps) ServeEcho(string) (func(), error)                 { return nil, errProbeLinuxOnly }
func (otherProbeOps) Probe(string, string, string) error               { return errProbeLinuxOnly }
func (otherProbeOps) LoadModules(names []string) []kmod.Result         { return nil }
func (otherProbeOps) WriteFile(string, []byte) error                   { return errProbeLinuxOnly }
func (otherProbeOps) RemoveAll(string) error                           { return nil }
func (otherProbeOps) InNetns(string, func() error) error               { return errProbeLinuxOnly }
