package fleet

import (
	"os"

	"github.com/hweeks/always-click-yes/internal/config"
)

// ForHost returns the Transport for running engineers on h: a local
// transport when h.SSH is empty, otherwise ssh to h.SSH. Both branches carry
// h.ACYBin through to the transport; a local host with no RepoPath (a
// FleetHost built by hand rather than through config.LoadFile, which already
// defaults it to the project dir) falls back to the current working
// directory, since that is what a bare `acy fleet doctor`/engineer launch
// from this machine actually means.
func ForHost(h config.FleetHost) Transport {
	acyBin := h.ACYBin
	if h.SSH != "" {
		if acyBin == "" {
			acyBin = "acy"
		}
		return &sshTransport{target: h.SSH, acyBin: acyBin, clonePath: h.RepoPath, path: h.Path}
	}

	if acyBin == "" {
		if exe, err := os.Executable(); err == nil {
			acyBin = exe
		} else {
			acyBin = "acy"
		}
	}
	repoPath := h.RepoPath
	if repoPath == "" {
		if wd, err := os.Getwd(); err == nil {
			repoPath = wd
		}
	}
	return &localTransport{acyBin: acyBin, clonePath: repoPath}
}
