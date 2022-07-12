/*
Copyright 2022.

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

package controllers

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/leseb/rook-s3-nano/api/v1alpha1"
	v1 "k8s.io/api/core/v1"
)

// normalizeKey converts a key in any format to a key with underscores.
//
// The internal representation of Ceph config keys uses underscores only, where Ceph supports both
// spaces, underscores, and hyphens. This is so that Rook can properly match and override keys even
// when they are specified as "some config key" in one section, "some_config_key" in another
// section, and "some-config-key" in yet another section.
func normalizeKey(key string) string {
	return strings.Replace(strings.Replace(key, " ", "_", -1), "-", "_", -1)
}

// ContainerEnvVarReference returns a reference to a Kubernetes container env var of the given name
// which can be used in command or argument fields.
func ContainerEnvVarReference(envVarName string) string {
	return fmt.Sprintf("$(%s)", envVarName)
}

func defaultDaemonFlag() []string {
	return append([]string{
		// Runs the daemon on stdout
		// Later when we log to file (and rotate it) we need to switch --foreground instead
		// In the meantime -d allows us to see all the logs
		// Daemonize option
		"-d",
		// Disable lockdep - might improve memory usage
		"--nolockdep",
	},
		defaultFlags()...)
}

// defaultFlags is used for both daemon and CLI
func defaultFlags() []string {
	return []string{
		// this is a must have since there is no ceph cluster to connect to.
		"--no-mon-config",

		newFlag("librados sqlite data dir", objectStoreDataDirectory),
		// flags to disable cephx (avoid cluttering logs)
		newFlag("auth-client-required", "none"),
		newFlag("auth-service-required", "none"),
		newFlag("auth-cluster-required", "none"),

		// dummy ceph.conf so that ceph doesn't complain about missing config file
		// Ceph does not care whether the content is correct or not
		newFlag("conf", "/etc/ceph/rbdmap"),
	}
}

func instanceName(name, namespace string) string {
	return fmt.Sprintf("%s-%s-%s", appName, name, namespace)
}

// newFlag returns the key-value pair in the format of a Ceph command line-compatible flag.
func newFlag(key, value string) string {
	// A flag is a normalized key with underscores replaced by dashes.
	// "debug default" ~normalize~> "debug_default" ~to~flag~> "debug-default"
	n := normalizeKey(key)
	f := strings.Replace(n, "_", "-", -1)
	return fmt.Sprintf("--%s=%s", f, value)
}

// buildFinalizerName returns the finalizer name
func buildFinalizerName(kind string) string {
	return fmt.Sprintf("%s.%s", strings.ToLower(kind), v1alpha1.GroupVersion)
}

// DaemonEnvVars Environment variables used by storage cluster daemon
func DaemonEnvVars(image string) []v1.EnvVar {
	return []v1.EnvVar{
		{Name: "CONTAINER_IMAGE", Value: image},
		{Name: "POD_NAME", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "POD_NAMESPACE", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		{Name: "NODE_NAME", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
		{Name: "CEPH_LIB", Value: "/usr/lib64/rados-classes"},
		// TODO: remove me once rgwam-sqlite supports all radosgw-sqlite-admin flags, currently if
		// fails with unknown args when passing --librados-sqlite-data-dir=/var/lib/ceph/radosgw/data
		{Name: "CEPH_ARGS", Value: "--librados-sqlite-data-dir=/var/lib/ceph/radosgw/data --no-mon-config --conf=/etc/ceph/rbdmap"},
	}
}

// DaemonVolumesDataPVC returns a PVC volume source for daemon container data.
func DaemonVolumesDataPVC(pvcName string) v1.Volume {
	return v1.Volume{
		Name: "ceph-daemon-data",
		VolumeSource: v1.VolumeSource{
			PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvcName,
			},
		},
	}
}

func daemonVolumeMountPVC() v1.VolumeMount {
	return v1.VolumeMount{
		Name:      "ceph-daemon-data",
		MountPath: objectStoreDataDirectory,
	}
}

// Hash stableName computes a stable pseudorandom string suitable for inclusion in a Kubernetes object name from the given seed string.
// Do **NOT** edit this function in a way that would change its output as it needs to
// provide consistent mappings from string to hash across versions of rook.
func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:16])
}

func buildFinalCLIArgs(args []string) []string {
	return append(defaultFlags(), args...)
}

// isBase64Encoded returns whether the keyring is valid
func isBase64Encoded(keyring string) bool {
	// If the keyring is not base64 we fail
	_, err := base64.StdEncoding.DecodeString(keyring)
	return err == nil
}
