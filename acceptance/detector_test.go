// +build acceptance

package acceptance

import (
	"context"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/buildpacks/lifecycle/cmd"

	"github.com/BurntSushi/toml"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/lifecycle"
	h "github.com/buildpacks/lifecycle/testhelpers"
)

var (
	detectDockerContext = filepath.Join("testdata", "detector")
	detectorBinaryDir   = filepath.Join("testdata", "detector", "container", "cnb", "lifecycle")
	detectImage         = "lifecycle/acceptance/detector"
	userID              string
	daemonOS            string
)

func TestDetector(t *testing.T) {
	rand.Seed(time.Now().UTC().UnixNano())

	info, err := h.DockerCli(t).Info(context.TODO())
	h.AssertNil(t, err)
	daemonOS = info.OSType

	dockerfilePath := filepath.Join(detectDockerContext, fmt.Sprintf("Dockerfile.%s", daemonOS))

	h.MakeAndCopyLifecycle(t, daemonOS, detectorBinaryDir)
	h.DockerBuild(t, detectImage, detectDockerContext, h.WithDockerfile(dockerfilePath))
	defer h.DockerImageRemove(t, detectImage)

	userID = "1234"
	if daemonOS == "windows" {
		userID = "ContainerUser"
	}
	spec.Run(t, "acceptance-detector", testDetector, spec.Parallel(), spec.Report(report.Terminal{}))
}

func testDetector(t *testing.T, when spec.G, it spec.S) {
	when("called with arguments", func() {
		it("errors", func() {
			command := exec.Command("docker", "run", "--rm", detectImage, "some-arg")
			output, err := command.CombinedOutput()
			h.AssertNotNil(t, err)
			expected := "failed to parse arguments: received unexpected arguments"
			h.AssertStringContains(t, string(output), expected)
		})
	})

	when("running as a root", func() {
		it.Focus("errors", func() {
			rootUser := "root"
			if daemonOS == "windows" {
				rootUser = "ContainerAdministrator"
			}

			command := exec.Command("docker", "run", "--rm", "--user", rootUser, detectImage)
			output, err := command.CombinedOutput()
			h.AssertNotNil(t, err)
			expected := "failed to build: refusing to run as root"
			h.AssertStringContains(t, string(output), expected)
		})
	})

	when("read buildpack order file failed", func() {
		it("errors", func() {
			// no order.toml file in the default directory
			command := exec.Command("docker", "run", "--rm", detectImage)
			output, err := command.CombinedOutput()
			h.AssertNotNil(t, err)
			expected := "failed to read buildpack order file"
			h.AssertStringContains(t, string(output), expected)
		})
	})

	when("no buildpack group passed detection", func() {
		it("errors", func() {
			command := exec.Command("docker", "run", "--rm", "--env", "CNB_ORDER_PATH=/cnb/orders/empty_order.toml", detectImage)
			output, err := command.CombinedOutput()
			h.AssertNotNil(t, err)
			failErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("expected an error of type exec.ExitError")
			}
			h.AssertEq(t, failErr.ExitCode(), cmd.CodeFailedDetect)
			expected := "No buildpack groups passed detection."
			h.AssertStringContains(t, string(output), expected)
		})
	})

	when("there is a buildpack group that pass detection", func() {
		var copyDir, containerName string

		it.Before(func() {
			containerName = "test-container-" + h.RandString(10)
			var err error
			copyDir, err = ioutil.TempDir("", "test-docker-copy-")
			h.AssertNil(t, err)
		})

		it.After(func() {
			if h.DockerContainerExists(t, containerName) {
				h.Run(t, exec.Command("docker", "rm", containerName))
			}
			os.RemoveAll(copyDir)
		})

		it("writes group.toml and plan.toml", func() {
			h.DockerRunAndCopy(t,
				containerName,
				copyDir,
				detectImage,
				"/layers",
				h.WithFlags("--user", userID,
					"--env", "CNB_ORDER_PATH=/cnb/orders/simple_order.toml",
				),
				h.WithArgs(),
			)

			// check group.toml
			tempGroupToml := filepath.Join(copyDir, "layers", "group.toml")
			var buildpackGroup lifecycle.BuildpackGroup
			_, err := toml.DecodeFile(tempGroupToml, &buildpackGroup)
			h.AssertNil(t, err)
			h.AssertEq(t, buildpackGroup.Group[0].ID, "simple_buildpack")
			h.AssertEq(t, buildpackGroup.Group[0].Version, "simple_buildpack_version")

			// check plan.toml
			tempPlanToml := filepath.Join(copyDir, "layers", "plan.toml")
			var buildPlan lifecycle.BuildPlan
			_, err = toml.DecodeFile(tempPlanToml, &buildPlan)
			h.AssertNil(t, err)
			h.AssertEq(t, buildPlan.Entries[0].Providers[0].ID, "simple_buildpack")
			h.AssertEq(t, buildPlan.Entries[0].Providers[0].Version, "simple_buildpack_version")
			h.AssertEq(t, buildPlan.Entries[0].Requires[0].Name, "some_requirement")
			h.AssertEq(t, buildPlan.Entries[0].Requires[0].Metadata["some_metadata_key"], "some_metadata_val")
			h.AssertEq(t, buildPlan.Entries[0].Requires[0].Metadata["version"], "some_version")
		})
	})

	when("environment variables are provided for buildpack and app directories and for the output files", func() {
		var copyDir, containerName string

		it.Before(func() {
			containerName = "test-container-" + h.RandString(10)
			var err error
			copyDir, err = ioutil.TempDir("", "test-docker-copy-")
			h.AssertNil(t, err)
		})

		it.After(func() {
			if h.DockerContainerExists(t, containerName) {
				h.Run(t, exec.Command("docker", "rm", containerName))
			}
			os.RemoveAll(copyDir)
		})

		it("writes group.toml and plan.toml in the right location and with the right names", func() {
			h.DockerRunAndCopy(t,
				containerName,
				copyDir,
				detectImage,
				"/layers",
				h.WithFlags("--user", userID,
					"--env", "CNB_ORDER_PATH=/cnb/orders/always_detect_order.toml",
					"--env", "CNB_BUILDPACKS_DIR=/cnb/custom_buildpacks",
					"--env", "CNB_APP_DIR=/custom_workspace",
					"--env", "CNB_GROUP_PATH=./custom_group.toml",
					"--env", "CNB_PLAN_PATH=./custom_plan.toml",
					"--env", "CNB_PLATFORM_DIR=/custom_platform",
				),
				h.WithArgs("-log-level=debug"),
			)

			// check group.toml
			tempGroupToml := filepath.Join(copyDir, "layers", "custom_group.toml")
			var buildpackGroup lifecycle.BuildpackGroup
			_, err := toml.DecodeFile(tempGroupToml, &buildpackGroup)
			h.AssertNil(t, err)
			h.AssertEq(t, buildpackGroup.Group[0].ID, "always_detect_buildpack")
			h.AssertEq(t, buildpackGroup.Group[0].Version, "always_detect_buildpack_version")

			// check plan.toml - should be empty since we're using always_detect_order.toml so there is no "actual plan"
			tempPlanToml := filepath.Join(copyDir, "layers", "custom_plan.toml")
			planContents, err := ioutil.ReadFile(tempPlanToml)
			h.AssertNil(t, err)
			h.AssertEq(t, len(planContents) == 0, true)

			// check platform directory
			logs := h.Run(t, exec.Command("docker", "logs", containerName))

			expectedPlatformPath := "platform_path: /custom_platform"
			expectedAppDir := "app_dir: /custom_workspace"
			if daemonOS == "windows" {
				expectedPlatformPath = `platform_path: C:\custom_platform`
				expectedAppDir = `app_dir: C:\custom_workspace`
			}

			h.AssertStringContains(t, logs, expectedPlatformPath)
			h.AssertStringContains(t, logs, expectedAppDir)
		})
	})
}
