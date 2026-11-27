package commands

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/jobs/commands/dependencies"
	"github.com/deployment-io/deployment-runner/jobs/resources"
	"github.com/deployment-io/deployment-runner/utils/reclaim"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// dependencyBumpDocker is the bump's Docker calls: the containers (pull,
// create, start, exec, remove, as the QA linker's) and its cache volume.
type dependencyBumpDocker struct {
	containers   *crossRepoDocker
	createVolume func(name string) error
	removeVolume func(name string)
}

func (b *dependencyBump) docker() *dependencyBumpDocker {
	if b.cmd.docker != nil {
		return b.cmd.docker
	}
	return &dependencyBumpDocker{
		containers:   defaultCrossRepoDocker(),
		createVolume: createDependencyBumpVolume,
		removeVolume: removeCacheVolume,
	}
}

// createDependencyBumpVolume creates the bump's module cache volume, labelled
// for the startup sweep.
func createDependencyBumpVolume(name string) error {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return err
	}
	defer cli.Close()
	_, err = cli.VolumeCreate(context.Background(), client.VolumeCreateOptions{
		Name:   name,
		Labels: reclaim.Labels(reclaim.PurposeDependencyBump),
	})
	return err
}

func (b *dependencyBump) removeVolume() {
	if b.volumeCreated {
		b.docker().removeVolume(b.volume)
		b.volumeCreated = false
	}
}

// Where the containers see the dependent, the mirrors and the cache.
const (
	dependencyBumpSrcMount     = "/src"
	dependencyBumpMirrorsMount = "/mirrors"
	dependencyBumpCacheMount   = "/cache"
)

// bumpGitEnv is the bump container's git settings: no system config, no
// prompt, the org's https URLs read from the mirrors, and any owner's
// repository trusted.
func (b *dependencyBump) bumpGitEnv() []string {
	return []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=url.file://" + dependencyBumpMirrorsMount + "/.insteadOf",
		"GIT_CONFIG_VALUE_0=https://" + b.scope.prefix() + "/",
		"GIT_CONFIG_KEY_1=safe.directory",
		"GIT_CONFIG_VALUE_1=*",
	}
}

// verifyGitEnv is the verify container's: no system config, no prompt, and
// /src trusted (go build stamps it with git).
func verifyGitEnv() []string {
	return []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=*",
	}
}

// dependencyBumpContainer is a container of cmds with env (only the
// adapter's and the git settings: no token, nothing inherited), as root in
// /src, with the static-site build container's limits, the cloud-metadata
// pins and the bump's reclaim label.
func dependencyBumpContainer(cmds dependencies.Commands, env []string, network string, mounts []mount.Mount) (*container.Config, *container.HostConfig) {
	memoryBytes, nanoCPUs := resources.LimitsForStaticSiteBuild()
	return &container.Config{
		Image:      cmds.Image,
		Cmd:        []string{"tail", "-f", "/dev/null"},
		User:       "root",
		WorkingDir: dependencyBumpSrcMount,
		Env:        append(append([]string{}, cmds.Env...), env...),
		Labels:     reclaim.Labels(reclaim.PurposeDependencyBump),
	}, &container.HostConfig{
		NetworkMode: container.NetworkMode(network),
		Mounts:      mounts,
		Resources: container.Resources{
			Memory:    memoryBytes,
			NanoCPUs:  nanoCPUs,
			PidsLimit: pidsLimitPtr(defaultBuildPidsLimit),
		},
		ExtraHosts: cloudMetadataExtraHosts(),
	}
}

// bumpContainer: network on, the dependent read-write, the mirrors
// read-only, the cache read-write.
func (b *dependencyBump) bumpContainer(cmds dependencies.Commands) (*container.Config, *container.HostConfig) {
	return dependencyBumpContainer(cmds, b.bumpGitEnv(), "bridge", []mount.Mount{
		{Type: mount.TypeBind, Source: b.repoDir(), Target: dependencyBumpSrcMount},
		{Type: mount.TypeBind, Source: filepath.Join(b.workDir, "mirrors"), Target: dependencyBumpMirrorsMount, ReadOnly: true},
		{Type: mount.TypeVolume, Source: b.volume, Target: dependencyBumpCacheMount},
	})
}

// verifyContainer: no network, the dependent and the cache read-only, so
// nothing the verify builds can end up in the commit.
func (b *dependencyBump) verifyContainer(cmds dependencies.Commands) (*container.Config, *container.HostConfig) {
	return dependencyBumpContainer(cmds, verifyGitEnv(), "none", []mount.Mount{
		{Type: mount.TypeBind, Source: b.repoDir(), Target: dependencyBumpSrcMount, ReadOnly: true},
		{Type: mount.TypeVolume, Source: b.volume, Target: dependencyBumpCacheMount, ReadOnly: true},
	})
}

// runContainer runs cmds in the container of cfg and hostCfg, its output to
// the job log; a failure is the error and the last output lines.
func (b *dependencyBump) runContainer(cmds dependencies.Commands, cfg *container.Config, hostCfg *container.HostConfig) error {
	tail := &tailWriter{max: crossRepoOutputMax}
	out := io.MultiWriter(&buildLogWriter{w: b.log}, tail)
	err := b.docker().containers.runContainer(context.Background(), cfg, hostCfg, cmds.Commands, out)
	if err == nil {
		return nil
	}
	reason := err.Error()
	if lines := lastLines(string(tail.buf), dependencyBumpOutputLines); lines != "" {
		reason += "\n" + lines
	}
	return fmt.Errorf("%s", reason)
}

// private is the module-name prefixes the mirrors serve.
func (b *dependencyBump) private() []string {
	return []string{b.scope.prefix()}
}

// runBumps is step 5: each ecosystem's bump commands in the bump container.
// A failure fails every target.
func (b *dependencyBump) runBumps() bool {
	if !b.volumeCreated {
		if err := b.docker().createVolume(b.volume); err != nil {
			b.failOpen(fmt.Sprintf("couldn't create the module cache: %s", err))
			return false
		}
		b.volumeCreated = true
	}
	for _, g := range b.groups() {
		bumps := make([]dependencies.Bump, 0, len(g.targets))
		for _, t := range g.targets {
			bumps = append(bumps, dependencies.Bump{Name: t.Name, Commit: t.Commit})
		}
		cmds, err := g.ecosystem.Bump(b.repoDir(), bumps, b.private())
		if err != nil {
			for _, t := range g.targets {
				b.fail(t, err.Error())
			}
			continue
		}
		b.logf("running %s in %s for %s", cmds.Summary, cmds.Image, b.entry.Name)
		cfg, hostCfg := b.bumpContainer(cmds)
		if err := b.runContainer(cmds, cfg, hostCfg); err != nil {
			b.failOpen(fmt.Sprintf("the %s bump failed: %s", g.ecosystem.Name(), err))
			return false
		}
		b.attemptState.ran = append(b.attemptState.ran, g.ecosystem)
	}
	return true
}

// runVerify is step 8: each ecosystem with a bumped target verifies, offline.
// A failure fails every target; nothing is pushed. In start-up mode it fails
// none: the bump is kept, with the build's error for the agent.
func (b *dependencyBump) runVerify() bool {
	verified := map[string]bool{}
	for _, t := range b.bumped() {
		eco := t.ecosystem
		if verified[eco.Name()] {
			continue
		}
		verified[eco.Name()] = true
		cmds, err := eco.Verify(b.repoDir(), b.private())
		if err == nil {
			b.logf("verifying %s with %s in %s", b.entry.Name, cmds.Summary, cmds.Image)
			cfg, hostCfg := b.verifyContainer(cmds)
			if err = b.runContainer(cmds, cfg, hostCfg); err != nil {
				err = fmt.Errorf("%s doesn't build after the bump: %s", b.entry.Name, err)
			}
		}
		if err == nil {
			continue
		}
		if b.stepDir == "" {
			b.failOpen(err.Error())
			return false
		}
		b.buildFailed(eco, err)
	}
	return true
}

// buildFailed is a verify failure in start-up mode: eco's bumped targets
// stay bumped and keep the error for the agent, who fixes the build.
func (b *dependencyBump) buildFailed(eco dependencies.Ecosystem, err error) {
	b.logf("%s doesn't build after the bump; keeping the bump for the agent", b.entry.Name)
	for _, t := range b.bumped() {
		if t.ecosystem.Name() == eco.Name() {
			t.buildError = truncateKeepingTail(b.scrub(err.Error()), tasks.DependencyErrorMaxBytes, "\n[...]\n")
		}
	}
}
