package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/cgroups/v3"
)

func main() {
	// Logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// Fetch required environment variables
	podID := getEnvOrExit("GITALY_POD_UID")
	cgroupPath := getEnvOrExit("CGROUP_PATH")
	outputPath := getEnvOrExit("OUTPUT_PATH")

	slog.Info("cgroup setup configuration",
		"GITALY_POD_UID", podID,
		"CGROUP_PATH", cgroupPath,
		"OUTPUT_PATH", outputPath,
	)

	// Validate cgroup mode/version, only v2 is supported
	if err := validateCgroupMode(); err != nil {
		slog.Error("failed to validate cgroup version", "error", err)
		os.Exit(1)
	}

	// Find Gitaly cgroup path
	k8sPodID := strings.Replace(podID, "-", "_", -1) // eg. 2953d396_fd5e_4fc6_910f_ab687c9d08a8
	podCgroupPath := findPodCgroup(cgroupPath, k8sPodID)
	if podCgroupPath == "" {
		slog.Error("failed to find cgroup path for Gitaly pod", "pod_uid", podID)
		os.Exit(1)
	}
	slog.Info("found cgroup path for Gitaly pod", "cgroup_path", podCgroupPath)

	// Update Gitaly pod cgroup permissions
	/* Minimal permissions that a pod will need in order to manage its own subtree.
	This is one way a cgroup can be delegated to a less privileged user, by granting write access
	of the directory and its "cgroup.procs", "cgroup.threads", "cgroup.subtree_control" files.
	Reference: https://docs.kernel.org/admin-guide/cgroup-v2.html#model-of-delegation
	*/
	modifyPaths := []string{"", "cgroup.procs", "cgroup.threads", "cgroup.subtree_control"}
	if err := changePermissions(podCgroupPath, modifyPaths); err != nil {
		slog.Error("changing cgroup permissions for Gitaly pod", "pod_uid", podID, "cgroup_path", podCgroupPath, "error", err)
		os.Exit(1)
	}
	slog.Info("changed cgroup permissions for Gitaly pod", "cgroup_path", podCgroupPath)

	// Write Gitaly pod cgroup path to an output file
	if err := writePodCgroupPath(outputPath, podCgroupPath); err != nil {
		slog.Error("failed to write pod cgroup path", "error", err)
		os.Exit(1)
	}
}

func getEnvOrExit(key string) string {
	value := os.Getenv(key)
	if value == "" {
		slog.Error("environment variable is not set", "variable", key)
		os.Exit(1)
	}
	return value
}

// validateCgroupMode verifies the running cgroup version/mode
func validateCgroupMode() error {
	switch cgroups.Mode() {
	case cgroups.Unified:
		return nil
	default:
		return errors.New("cgroup v2 (unified) wasn't detected, it is required")
	}
}

// findPodCgroup finds the path to the pod's cgroup given the pod uid
func findPodCgroup(path string, uid string) string {
	directory := filepath.Join(path)

	// QoS guaranteed pods are created directly under `kubepods.slice`
	// `Glob()` doesn't support `**`, running two instances
	if files, _ := filepath.Glob(directory + "/*-pod" + uid + ".slice"); len(files) > 0 {
		return files[0]
	}

	// QoS burstable + besteffort pods are created under `kubepods-burstable.slice` and `kubepods-besteffort.slice`
	if files, _ := filepath.Glob(directory + "/*/*-pod" + uid + ".slice"); len(files) > 0 {
		return files[0]
	}

	return ""
}

// changePermissions changes permissions of cgroup paths enabling write access for the Gitaly pod
func changePermissions(podCgroupPath string, paths []string) error {
	for _, path := range paths {
		filePath := filepath.Join(podCgroupPath, path)
		if err := os.Chown(filePath, 1000, 1000); err != nil {
			return fmt.Errorf("chown cgroup path %q: %w", path, err)
		}
	}
	return nil
}

// writePodCgroupPath outputs the pod level cgroup path to a file
func writePodCgroupPath(outputPath string, podCgroupPath string) error {
	return os.WriteFile(outputPath, []byte(podCgroupPath), 0o644)
}
