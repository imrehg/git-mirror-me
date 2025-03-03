// SPDX-FileCopyrightText: Andrei Gherzan <andrei@gherzan.com>
//
// SPDX-License-Identifier: MIT

package mirror

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
)

const (
	refsFilterPrefix       = "refs/pull"
	srcRemoteName          = "src"
	dstRemoteName          = "dst"
	tmpKnownHostPathPrefix = "git-mirror-me-known_hosts-"
	knownHostsPerm         = 0o600
)

// FilterOutRefs takes a repository and removes references based on a slice of
// prefixes.
func filterOutRefs(repo *git.Repository, prefixes []string) error {
	if len(prefixes) == 0 {
		return nil
	}

	refs, err := repo.References()
	if err != nil {
		return fmt.Errorf("failed to get references: %w", err)
	}

	if err = refs.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().String()
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				if err := repo.Storer.RemoveReference(ref.Name()); err != nil {
					return fmt.Errorf("failed to remove reference: %w", err)
				}

				break
			}
		}

		return nil
	}); err != nil {
		return fmt.Errorf("failed remove references: %w", err)
	}

	return nil
}

// refsToDeleteSpecs returns a slice of delete refspecs for a slice of
// references.
func refsToDeleteSpecs(refs []*plumbing.Reference) []config.RefSpec {
	specs := make([]config.RefSpec, 0, len(refs))
	for _, ref := range refs {
		specs = append(specs, config.RefSpec(":"+ref.Name().String()))
	}

	return specs
}

// extraRefs returns a slice of references that are in refs but not in the
// repository.
func extraRefs(repo *git.Repository, refs []*plumbing.Reference) ([]*plumbing.Reference, error) {
	var retRefs []*plumbing.Reference

	for _, ref := range refs {
		repoRefs, err := repo.References()
		if err != nil {
			return nil, fmt.Errorf("failed to get references: %w", err)
		}

		found := false

		_ = repoRefs.ForEach(func(repoRef *plumbing.Reference) error {
			if repoRef.Name().String() == ref.Name().String() {
				found = true
			}

			return nil
		})

		if !found {
			retRefs = append(retRefs, ref)
		}
	}

	return retRefs, nil
}

// extraSpecs takes a repository and a slice of refs and returns the refs
// that are not in the repository as a slice of delete refspecs.
func extraSpecs(repo *git.Repository, refs []*plumbing.Reference) ([]config.RefSpec, error) {
	diffRefs, err := extraRefs(repo, refs)
	if err != nil {
		return nil, err
	}

	return refsToDeleteSpecs(diffRefs), nil
}

// pruneRemote removes all the references in a remote that are not available in
// the repo.
func pruneRemote(remote *git.Remote, auth transport.AuthMethod, repo *git.Repository) error {
	refs, err := remote.List(&git.ListOptions{
		Auth: auth,
	})
	if err != nil {
		return fmt.Errorf("failed to list the destination remote: %w", err)
	}

	deleteSpecs, err := extraSpecs(repo, refs)
	if err != nil {
		return fmt.Errorf("failed to get the prune specs: %w", err)
	}

	if len(deleteSpecs) > 0 {
		err := remote.Push(&git.PushOptions{
			RemoteName: remote.Config().Name,
			Auth:       auth,
			RefSpecs:   deleteSpecs,
		})
		if err != nil && errors.Is(err, git.NoErrAlreadyUpToDate) {
			return fmt.Errorf("failed to prune destination: %w", err)
		}
	}

	return nil
}

// setupStagingRepo initialises an in-memory git repositry populated with the
// source's references.
func setupStagingRepo(conf Config, logger *Logger) (*git.Repository, error) {
	// Setup a working repository.
	logger.Info("Setting up a staging git repository.")

	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed initialising staging git repository: %w",
			err)
	}

	// Set up the source remote.
	src, err := repo.CreateRemote(&config.RemoteConfig{
		Name: srcRemoteName,
		URLs: []string{conf.SrcRepo},
	})
	if err != nil {
		return nil, fmt.Errorf("failed configuring source remote: %w", err)
	}

	// Fetch the source.
	logger.Info("Fetching all refs from", conf.SrcRepo, "...")

	if err := src.Fetch(&git.FetchOptions{
		RemoteName: srcRemoteName,
		RefSpecs:   []config.RefSpec{"refs/*:refs/*"},
	}); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, fmt.Errorf("failed to fetch source remote: %w", err)
	}

	return repo, nil
}

// pushWithAuth sets authentication based on configuration and pushes all
// references to the configured destination repository (as a mirror).
func pushWithAuth(conf Config, logger *Logger, stagingRepo *git.Repository) error {
	var auth transport.AuthMethod

	// Set up the public host key.
	//
	// The host public keys can be provided via both content and path. When
	// it is provided via content, we need to use a temporary known_hosts
	// file.
	knownHostsPath := conf.GetKnownHostsPath()

	if len(conf.SSH.KnownHosts) != 0 {
		knownHostsFile, err := ioutil.TempFile("/tmp", tmpKnownHostPathPrefix)
		if err != nil {
			return fmt.Errorf("error creating known_hosts tmp file: %w", err)
		}

		defer func() {
			knownHostsFile.Close()
			os.Remove(knownHostsFile.Name())
		}()

		knownHostsPath = knownHostsFile.Name()

		err = os.WriteFile(knownHostsPath, []byte(conf.SSH.KnownHosts), knownHostsPerm)
		if err != nil {
			return fmt.Errorf("error writing known_hosts tmp file: %w", err)
		}
	}

	// Set up SSH authentication.
	if len(conf.SSH.PrivateKey) > 0 {
		logger.Debug(conf.Debug, "Using SSH authentication.")

		sshKeys, err := ssh.NewPublicKeys("git", []byte(conf.SSH.PrivateKey), "")
		if err != nil {
			return fmt.Errorf("failed to setup the SSH key: %w", err)
		}

		hostKeyCallback, err := ssh.NewKnownHostsCallback(knownHostsPath)
		if err != nil {
			return fmt.Errorf("failed to set up host keys: %w", err)
		}

		hostKeyCallbackHelper := ssh.HostKeyCallbackHelper{
			HostKeyCallback: hostKeyCallback,
		}
		sshKeys.HostKeyCallbackHelper = hostKeyCallbackHelper
		auth = sshKeys
	}

	// Set up the destination remote.
	dst, err := stagingRepo.CreateRemote(&config.RemoteConfig{
		Name: dstRemoteName,
		URLs: []string{conf.DstRepo},
	})
	if err != nil {
		return fmt.Errorf("failed configuring destination remote: %w", err)
	}

	logger.Info("Pushing to destination...")

	err = dst.Push(&git.PushOptions{
		RemoteName: dstRemoteName,
		Auth:       auth,
		RefSpecs:   []config.RefSpec{"refs/*:refs/*"},
		Force:      true,
		Prune:      false, // https://github.com/go-git/go-git/issues/520
	})
	if err != nil {
		switch {
		case errors.Is(err, git.NoErrAlreadyUpToDate):
			logger.Info("Destination already up to date.")
		default:
			return fmt.Errorf("failed to push to destination: %w", err)
		}
	} else {
		logger.Info("Successfully mirrored pushed to destination repository.")
	}

	// We can not use prune in git.Push due to an existing bug
	// https://github.com/go-git/go-git/issues/520 so we workaround it dealing
	// with the prunning with a separate push.
	logger.Info("Pruning the destination...")

	err = pruneRemote(dst, auth, stagingRepo)
	if err != nil {
		return nil
	}

	return nil
}

// DoMirror mirrors the source to the destination git repository based on the
// provided configuration. Special references (for example GitHub's
// refs/pull/*) are ignored.
func DoMirror(conf Config, logger *Logger) error {
	repo, err := setupStagingRepo(conf, logger)
	if err != nil {
		return err
	}

	// Do not push GitHub special references used for dealing with pull
	// requests.
	if err := filterOutRefs(repo, []string{refsFilterPrefix}); err != nil {
		return fmt.Errorf("failed to filter out the refs: %w", err)
	}

	if err := pushWithAuth(conf, logger, repo); err != nil {
		return err
	}

	return nil
}
