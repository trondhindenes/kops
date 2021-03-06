/*
Copyright 2019 The Kubernetes Authors.

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

package model

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/blang/semver/v4"
	"k8s.io/klog/v2"
	"k8s.io/kops/nodeup/pkg/model/resources"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/flagbuilder"
	"k8s.io/kops/pkg/model/components"
	"k8s.io/kops/pkg/systemd"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/nodeup/nodetasks"
	"k8s.io/kops/util/pkg/distributions"
)

// ContainerdBuilder install containerd (just the packages at the moment)
type ContainerdBuilder struct {
	*NodeupModelContext
}

var _ fi.ModelBuilder = &ContainerdBuilder{}

// Build is responsible for configuring the containerd daemon
func (b *ContainerdBuilder) Build(c *fi.ModelBuilderContext) error {
	if b.skipInstall() {
		klog.Infof("SkipInstall is set to true; won't install containerd")
		return nil
	}

	// @check: neither flatcar nor containeros need provision containerd.service, just the containerd daemon options
	switch b.Distribution {
	case distributions.DistributionFlatcar:
		klog.Infof("Detected Flatcar; won't install containerd")
		if b.Cluster.Spec.ContainerRuntime == "containerd" {
			b.buildSystemdServiceOverrideFlatcar(c)
			b.buildConfigFile(c)
		}
		return nil
	case distributions.DistributionContainerOS:
		klog.Infof("Detected ContainerOS; won't install containerd")
		b.buildSystemdServiceOverrideContainerOS(c)
		return nil
	}

	// Add Apache2 license
	{
		t := &nodetasks.File{
			Path:     "/usr/share/doc/containerd/apache.txt",
			Contents: fi.NewStringResource(resources.ContainerdApache2License),
			Type:     nodetasks.FileType_File,
		}
		c.AddTask(t)
	}

	// Add config file
	b.buildConfigFile(c)

	// Add binaries from assets
	if b.Cluster.Spec.ContainerRuntime == "containerd" {
		f := b.Assets.FindMatches(regexp.MustCompile(`^(\./)?usr/local/(bin/containerd|bin/crictl|bin/ctr|sbin/runc)`))
		if len(f) == 0 {
			f = b.Assets.FindMatches(regexp.MustCompile(`^docker/(containerd|ctr|runc)`))
		}
		if len(f) == 0 {
			return fmt.Errorf("unable to find any containerd binaries in assets")
		}
		for k, v := range f {
			fileTask := &nodetasks.File{
				Path:     filepath.Join("/usr/bin", k),
				Contents: v,
				Type:     nodetasks.FileType_File,
				Mode:     fi.String("0755"),
			}
			c.AddTask(fileTask)
		}

		// Add configuration file for easier use of crictl
		b.addCrictlConfig(c)

		// Using containerd with Kubenet requires special configuration.
		// This is a temporary backwards-compatible solution for kubenet users and will be deprecated when Kubenet is deprecated:
		// https://github.com/containerd/containerd/blob/master/docs/cri/config.md#cni-config-template
		if components.UsesKubenet(b.Cluster.Spec.Networking) {
			b.buildCNIConfigTemplateFile(c)
		}

	}

	var containerRuntimeVersion string
	if b.Cluster.Spec.ContainerRuntime == "containerd" {
		if b.Cluster.Spec.Containerd != nil {
			containerRuntimeVersion = fi.StringValue(b.Cluster.Spec.Containerd.Version)
		} else {
			return fmt.Errorf("error finding contained version")
		}
	} else {
		if b.Cluster.Spec.Docker != nil {
			containerRuntimeVersion = fi.StringValue(b.Cluster.Spec.Docker.Version)
		} else {
			return fmt.Errorf("error finding Docker version")
		}
	}
	sv, err := semver.ParseTolerant(containerRuntimeVersion)
	if err != nil {
		return fmt.Errorf("error parsing container runtime version %q: %v", containerRuntimeVersion, err)
	}
	c.AddTask(b.buildSystemdService(sv))

	if err := b.buildSysconfigFile(c); err != nil {
		return err
	}

	return nil
}

func (b *ContainerdBuilder) buildSystemdService(sv semver.Version) *nodetasks.Service {
	// Based on https://github.com/containerd/containerd/blob/master/containerd.service

	manifest := &systemd.Manifest{}
	manifest.Set("Unit", "Description", "containerd container runtime")
	manifest.Set("Unit", "Documentation", "https://containerd.io")
	manifest.Set("Unit", "After", "network.target local-fs.target")

	// Restore the default SELinux security contexts for the containerd and runc binaries
	if b.Distribution.IsRHELFamily() && b.Cluster.Spec.Docker != nil && fi.BoolValue(b.Cluster.Spec.Docker.SelinuxEnabled) {
		manifest.Set("Service", "ExecStartPre", "/bin/sh -c 'restorecon -v /usr/bin/runc'")
		manifest.Set("Service", "ExecStartPre", "/bin/sh -c 'restorecon -v /usr/bin/containerd*'")
	}

	manifest.Set("Service", "EnvironmentFile", "/etc/sysconfig/containerd")
	manifest.Set("Service", "EnvironmentFile", "/etc/environment")
	manifest.Set("Service", "ExecStartPre", "-/sbin/modprobe overlay")
	manifest.Set("Service", "ExecStart", "/usr/bin/containerd -c /etc/containerd/config-kops.toml \"$CONTAINERD_OPTS\"")

	// notify the daemon's readiness to systemd
	if (b.Cluster.Spec.ContainerRuntime == "containerd" && sv.GTE(semver.MustParse("1.3.4"))) || sv.GTE(semver.MustParse("19.3.13")) {
		manifest.Set("Service", "Type", "notify")
	}

	// set delegate yes so that systemd does not reset the cgroups of containerd containers
	manifest.Set("Service", "Delegate", "yes")
	// kill only the containerd process, not all processes in the cgroup
	manifest.Set("Service", "KillMode", "process")

	manifest.Set("Service", "Restart", "always")
	manifest.Set("Service", "RestartSec", "5")

	manifest.Set("Service", "LimitNPROC", "infinity")
	manifest.Set("Service", "LimitCORE", "infinity")
	manifest.Set("Service", "LimitNOFILE", "infinity")
	manifest.Set("Service", "TasksMax", "infinity")

	// make killing of processes of this unit under memory pressure very unlikely
	manifest.Set("Service", "OOMScoreAdjust", "-999")

	manifest.Set("Install", "WantedBy", "multi-user.target")

	manifestString := manifest.Render()
	klog.V(8).Infof("Built service manifest %q\n%s", "containerd", manifestString)

	service := &nodetasks.Service{
		Name:       "containerd.service",
		Definition: s(manifestString),
	}

	service.InitDefaults()

	return service
}

// buildSystemdServiceOverrideContainerOS is responsible for overriding the containerd service for ContainerOS
func (b *ContainerdBuilder) buildSystemdServiceOverrideContainerOS(c *fi.ModelBuilderContext) {
	lines := []string{
		"[Service]",
		"EnvironmentFile=/etc/environment",
		"TasksMax=infinity",
	}
	contents := strings.Join(lines, "\n")

	c.AddTask(&nodetasks.File{
		Path:     "/etc/systemd/system/containerd.service.d/10-kops.conf",
		Contents: fi.NewStringResource(contents),
		Type:     nodetasks.FileType_File,
		OnChangeExecute: [][]string{
			{"systemctl", "daemon-reload"},
			{"systemctl", "restart", "containerd.service"},
			// We need to restart kops-configuration service since nodeup needs to load images
			// into containerd with the new config. We restart in the background because
			// kops-configuration is of type "one-shot", so the restart command will wait for
			// nodeup to finish executing.
			{"systemctl", "restart", "kops-configuration.service", "&"},
		},
	})
}

// buildSystemdServiceOverrideFlatcar is responsible for overriding the containerd service for Flatcar
func (b *ContainerdBuilder) buildSystemdServiceOverrideFlatcar(c *fi.ModelBuilderContext) {
	lines := []string{
		"[Service]",
		"Environment=CONTAINERD_CONFIG=/etc/containerd/config-kops.toml",
		"EnvironmentFile=/etc/environment",
	}
	contents := strings.Join(lines, "\n")

	c.AddTask(&nodetasks.File{
		Path:     "/etc/systemd/system/containerd.service.d/10-kops.conf",
		Contents: fi.NewStringResource(contents),
		Type:     nodetasks.FileType_File,
		OnChangeExecute: [][]string{
			{"systemctl", "daemon-reload"},
			{"systemctl", "restart", "containerd.service"},
			// We need to restart kops-configuration service since nodeup needs to load images
			// into containerd with the new config. We restart in the background because
			// kops-configuration is of type "one-shot", so the restart command will wait for
			// nodeup to finish executing.
			{"systemctl", "restart", "kops-configuration.service", "&"},
		},
	})
}

// buildSysconfigFile is responsible for creating the containerd sysconfig file
func (b *ContainerdBuilder) buildSysconfigFile(c *fi.ModelBuilderContext) error {
	var containerd kops.ContainerdConfig
	if b.Cluster.Spec.Containerd != nil {
		containerd = *b.Cluster.Spec.Containerd
	}

	flagsString, err := flagbuilder.BuildFlags(&containerd)
	if err != nil {
		return fmt.Errorf("error building containerd flags: %v", err)
	}

	lines := []string{
		"CONTAINERD_OPTS=" + flagsString,
	}
	contents := strings.Join(lines, "\n")

	c.AddTask(&nodetasks.File{
		Path:     "/etc/sysconfig/containerd",
		Contents: fi.NewStringResource(contents),
		Type:     nodetasks.FileType_File,
	})

	return nil
}

// buildConfigFile is responsible for creating the containerd configuration file
func (b *ContainerdBuilder) buildConfigFile(c *fi.ModelBuilderContext) {
	containerdConfigOverride := ""
	if b.Cluster.Spec.Containerd != nil {
		containerdConfigOverride = fi.StringValue(b.Cluster.Spec.Containerd.ConfigOverride)
	}

	c.AddTask(&nodetasks.File{
		Path:     "/etc/containerd/config-kops.toml",
		Contents: fi.NewStringResource(containerdConfigOverride),
		Type:     nodetasks.FileType_File,
	})
}

// skipInstall determines if kops should skip the installation and configuration of containerd
func (b *ContainerdBuilder) skipInstall() bool {
	d := b.Cluster.Spec.Containerd

	// don't skip install if the user hasn't specified anything
	if d == nil {
		return false
	}

	return d.SkipInstall
}

// addCritctlConfig creates /etc/crictl.yaml, which lets crictl work out-of-the-box.
func (b *ContainerdBuilder) addCrictlConfig(c *fi.ModelBuilderContext) {
	conf := `
runtime-endpoint: unix:///run/containerd/containerd.sock
`

	c.AddTask(&nodetasks.File{
		Path:     "/etc/crictl.yaml",
		Contents: fi.NewStringResource(conf),
		Type:     nodetasks.FileType_File,
	})
}

// buildCNIConfigTemplateFile is responsible for creating a special template for setups using Kubenet
func (b *ContainerdBuilder) buildCNIConfigTemplateFile(c *fi.ModelBuilderContext) {
	contents := `{
    "cniVersion": "0.4.0",
    "name": "containerd-net",
    "plugins": [
        {
            "type": "bridge",
            "bridge": "cni0",
            "isGateway": true,
            "ipMasq": true,
            "promiscMode": true,
            "ipam": {
                "type": "host-local",
                "ranges": [[{"subnet": "{{.PodCIDR}}"}]],
                "routes": [{ "dst": "0.0.0.0/0" }]
            }
        },
        {
            "type": "portmap",
            "capabilities": {"portMappings": true}
        }
    ]
}
`
	klog.V(8).Infof("Built containerd CNI config template\n%s", contents)

	c.AddTask(&nodetasks.File{
		Path:     "/etc/containerd/config-cni.template",
		Contents: fi.NewStringResource(contents),
		Type:     nodetasks.FileType_File,
	})
}
