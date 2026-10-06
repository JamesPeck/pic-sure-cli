package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// VersionInfo is the part of `docker version --format json` the CLI uses.
type VersionInfo struct {
	Client ClientVersion
	// Server is nil when the daemon couldn't be reached.
	Server *ServerVersion
}

// ClientVersion is the docker CLI's half of VersionInfo.
type ClientVersion struct {
	Version    string
	APIVersion string `json:"ApiVersion"`
	OS         string `json:"Os"`
	Arch       string
	// Context is the active docker context, e.g. "desktop-linux".
	Context string
}

// ServerVersion is the daemon's half of VersionInfo.
type ServerVersion struct {
	// Platform names the product, e.g. "Docker Desktop 4.85.0 (235549)".
	Platform      Platform
	Version       string
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string
	OS            string `json:"Os"`
	Arch          string
	KernelVersion string
	Components    []Component
}

// Platform names a docker product.
type Platform struct{ Name string }

// Component is one versioned part of the daemon: Engine, containerd, runc
// and so on.
type Component struct {
	Name    string
	Version string
}

// Info is the part of `docker info --format json` the CLI uses.
type Info struct {
	ServerVersion string
	// OperatingSystem is e.g. "Docker Desktop", "OrbStack" or
	// "Ubuntu 24.04.1 LTS".
	OperatingSystem string
	OSType          string
	// Architecture is the daemon's, e.g. "aarch64" or "x86_64".
	Architecture  string
	KernelVersion string
	NCPU          int
	// MemTotal is the memory available to containers, in bytes: the VM's
	// memory under Docker Desktop, Colima or OrbStack.
	MemTotal        int64
	DockerRootDir   string
	Name            string
	SecurityOptions []string
	Labels          []string
	HTTPProxy       string `json:"HttpProxy"`
	HTTPSProxy      string `json:"HttpsProxy"`
	NoProxy         string
	Warnings        []string
	ClientInfo      ClientInfo
	// ServerErrors, when not empty, says the daemon couldn't be reached,
	// for a CLI that reports that here and exits 0.
	ServerErrors []string
}

// ClientInfo is the CLI's part of Info.
type ClientInfo struct {
	Context string
	// Plugins are the CLI plugins, such as compose and buildx.
	Plugins  []Plugin
	Warnings []string
}

// Plugin is a docker CLI plugin.
type Plugin struct {
	Name string
	// Version is as the plugin reports it, e.g. "v2.29.7" or
	// "v0.35.0-desktop.2".
	Version string
	Path    string
}

func (e *cliEngine) Version(ctx context.Context) (VersionInfo, error) {
	argv := []string{"docker", "version", "--format", "json"}
	res, err := e.r.Run(ctx, Cmd{Argv: argv})
	if err != nil {
		return VersionInfo{}, err
	}
	var v VersionInfo
	perr := decodeJSON(res.Stdout, &v)
	if res.ExitCode != 0 {
		err := exitError(argv, res.ExitCode, res.Stderr)
		if perr == nil && v.Server == nil {
			return v, daemonError{err}
		}
		return v, err
	}
	if perr != nil {
		return v, fmt.Errorf("parsing docker version: %w", perr)
	}
	return v, nil
}

func (e *cliEngine) Info(ctx context.Context) (Info, error) {
	argv := []string{"docker", "info", "--format", "json"}
	res, err := e.r.Run(ctx, Cmd{Argv: argv})
	if err != nil {
		return Info{}, err
	}
	var info Info
	perr := decodeJSON(res.Stdout, &info)
	switch {
	case res.ExitCode != 0:
		err := exitError(argv, res.ExitCode, res.Stderr)
		if perr == nil && info.ServerVersion == "" {
			return info, daemonError{err}
		}
		return info, err
	case perr != nil:
		return info, fmt.Errorf("parsing docker info: %w", perr)
	case len(info.ServerErrors) > 0:
		return info, daemonError{errors.New(info.ServerErrors[0])}
	}
	return info, nil
}

// decodeJSON is json.Unmarshal that also fails on empty output.
func decodeJSON(out []byte, v any) error {
	if len(bytes.TrimSpace(out)) == 0 {
		return errors.New("no output")
	}
	return json.Unmarshal(out, v)
}
