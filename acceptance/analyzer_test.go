package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/lifecycle"
	"github.com/buildpacks/lifecycle/auth"
	h "github.com/buildpacks/lifecycle/testhelpers"
)

var (
	analyzerBinaryDir    = filepath.Join("testdata", "analyzer", "analyze-image", "container", "cnb", "lifecycle")
	analyzeDockerContext = filepath.Join("testdata", "analyzer", "analyze-image")
	analyzeImage         = "lifecycle/acceptance/analyzer"
	analyzerPath         = "/cnb/lifecycle/analyzer"
	cacheFixtureDir      = filepath.Join("testdata", "analyzer", "cache-dir")
	daemonOS             string
)

var (
	registry *h.DockerRegistry
)

func TestAnalyzer(t *testing.T) {
	rand.Seed(time.Now().UTC().UnixNano())

	info, err := h.DockerCli(t).Info(context.TODO())
	h.AssertNil(t, err)
	daemonOS = info.OSType

	// Setup registry

	dockerConfigDir, err := ioutil.TempDir("", "test.docker.config.dir")
	h.AssertNil(t, err)
	defer os.RemoveAll(dockerConfigDir)

	registry = h.NewDockerRegistryWithAuth(dockerConfigDir)
	registry.Start(t)
	defer registry.Stop(t)

	os.Setenv("DOCKER_CONFIG", registry.DockerDirectory)

	// Setup test container

	h.MakeAndCopyLifecycle(t, "linux", analyzerBinaryDir)
	h.DockerBuild(t, analyzeImage, analyzeDockerContext)
	defer h.DockerImageRemove(t, analyzeImage)

	spec.Run(t, "acceptance-analyzer", testAnalyzer, spec.Parallel(), spec.Report(report.Terminal{}))
}

func testAnalyzer(t *testing.T, when spec.G, it spec.S) {
	when("called without an app image", func() {
		it("errors", func() {
			cmd := exec.Command(
				"docker", "run", "--rm",
				"--env", "CNB_REGISTRY_AUTH={}",
				analyzeImage,
				analyzerPath,
			)
			output, err := cmd.CombinedOutput()

			h.AssertNotNil(t, err)
			expected := "failed to parse arguments: received 0 arguments, but expected 1"
			h.AssertStringContains(t, string(output), expected)
		})
	})

	when("cache image tag and cache directory are both blank", func() {
		it("warns", func() {
			output := h.DockerRun(t,
				analyzeImage,
				h.WithFlags("--env", "CNB_REGISTRY_AUTH={}"),
				h.WithArgs(analyzerPath, "some-image"),
			)

			expected := "Not restoring cached layer metadata, no cache flag specified."
			h.AssertStringContains(t, output, expected)
		})
	})

	when("the provided layers directory isn't writeable", func() {
		it("recursively chowns the directory", func() {
			output := h.DockerRun(t,
				analyzeImage,
				h.WithFlags("--env", "CNB_REGISTRY_AUTH={}"),
				h.WithBash(fmt.Sprintf("chown -R 9999:9999 /layers; chmod -R 775 /layers; %s some-image; ls -al /layers", analyzerPath)),
			)

			h.AssertMatch(t, output, "2222 3333 .+ \\.")
			h.AssertMatch(t, output, "2222 3333 .+ group.toml")
		})
	})

	when("group path is provided", func() {
		it("uses the provided group path", func() {
			cacheVolume := h.SeedDockerVolume(t, cacheFixtureDir, daemonOS)
			defer h.DockerVolumeRemove(t, cacheVolume)

			_, tempDir := h.DockerRunAndCopy(t,
				analyzeImage,
				"/layers",
				h.WithFlags(
					"--env", "CNB_REGISTRY_AUTH={}",
					"--volume", fmt.Sprintf("%s:/cache", cacheVolume), // use a cache so that we can observe the effect of other-group.toml on /layers
				),
				h.WithArgs(
					analyzerPath,
					"-cache-dir", "/cache",
					"-group", "/layers/other-group.toml",
					"some-image",
				),
			)
			defer os.RemoveAll(tempDir)

			h.AssertPathExists(t, filepath.Join(tempDir, "layers", "some-other-buildpack-id"))
			h.AssertPathDoesNotExist(t, filepath.Join(tempDir, "layers", "some-buildpack-id"))
		})
	})

	when("layers path and analyzed path are provided", func() {
		it("writes analyzed.toml at the provided path", func() {
			_, tempDir := h.DockerRunAndCopy(t,
				analyzeImage,
				"/other-layers/other-analyzed.toml",
				h.WithFlags("--env", "CNB_REGISTRY_AUTH={}"),
				h.WithArgs(
					analyzerPath,
					"-layers", "/other-layers",
					"-analyzed", "/other-layers/other-analyzed.toml",
					"some-image",
				),
			)
			defer os.RemoveAll(tempDir)

			assertAnalyzedMetadata(t, filepath.Join(tempDir, "other-analyzed.toml"))
		})
	})

	when("daemon case", func() {
		it("writes analyzed.toml", func() {
			_, tempDir := h.DockerRunAndCopy(t,
				analyzeImage,
				"/layers/analyzed.toml",
				h.WithFlags("--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock"),
				h.WithArgs(analyzerPath, "-daemon", "some-image"),
			)
			defer os.RemoveAll(tempDir)

			assertAnalyzedMetadata(t, filepath.Join(tempDir, "analyzed.toml"))
		})

		when("app image is found", func() {
			var appImage string

			it.Before(func() {
				appImage = "some-app-image-" + h.RandString(10)
				metadata := flattenMetadata(t, filepath.Join("testdata", "analyzer", "app_image_metadata.json"), lifecycle.LayersMetadata{})

				cmd := exec.Command(
					"docker",
					"build",
					"-t", appImage,
					"--build-arg", fmt.Sprintf("metadata=%s", metadata),
					filepath.Join("testdata", "analyzer", "app-image"),
				)
				h.Run(t, cmd)
			})

			it.After(func() {
				h.DockerImageRemove(t, appImage)
			})

			it("restores layer metadata", func() {
				_, tempDir := h.DockerRunAndCopy(t,
					analyzeImage,
					"/layers",
					h.WithFlags("--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock"),
					h.WithArgs(analyzerPath, "-daemon", appImage),
				)
				defer os.RemoveAll(tempDir)

				layerFilenames := []string{
					"launch-build-cache-layer.sha",
					"launch-build-cache-layer.toml",
					"launch-cache-layer.sha",
					"launch-cache-layer.toml",
					"launch-layer.sha",
					"launch-layer.toml",
					"store.toml",
				}
				for _, filename := range layerFilenames {
					h.AssertPathExists(t, filepath.Join(tempDir, "layers", "some-buildpack-id", filename))
				}
			})

			when("skip layers is provided", func() {
				it("writes analyzed.toml and does not write buildpack layer metadata", func() {
					_, tempDir := h.DockerRunAndCopy(t,
						analyzeImage,
						"/layers",
						h.WithFlags("--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock"),
						h.WithArgs(
							analyzerPath,
							"-daemon",
							"-skip-layers",
							appImage,
						),
					)
					defer os.RemoveAll(tempDir)

					assertAnalyzedMetadata(t, filepath.Join(tempDir, "layers", "analyzed.toml"))
					h.AssertPathExists(t, filepath.Join(tempDir, "layers", "some-buildpack-id", "store.toml"))
					layerFilenames := []string{
						"launch-build-cache-layer.sha",
						"launch-build-cache-layer.toml",
						"launch-cache-layer.sha",
						"launch-cache-layer.toml",
						"launch-layer.sha",
						"launch-layer.toml",
					}
					for _, filename := range layerFilenames {
						h.AssertPathDoesNotExist(t, filepath.Join(tempDir, "layers", "some-buildpack-id", filename))
					}
				})
			})
		})

		when("cache is provided", func() {
			when("cache image case", func() {
				var cacheImage string

				it.Before(func() {
					metadata := flattenMetadata(t, filepath.Join("testdata", "analyzer", "cache_image_metadata.json"), lifecycle.CacheMetadata{})
					cacheImage = "some-cache-image-" + h.RandString(10)

					cmd := exec.Command(
						"docker",
						"build",
						"-t", cacheImage,
						"--build-arg", fmt.Sprintf("metadata=%s", metadata),
						filepath.Join("testdata", "analyzer", "cache-image"),
					)
					h.Run(t, cmd)
				})

				it.After(func() {
					h.DockerImageRemove(t, cacheImage)
				})

				it("ignores the cache", func() {
					_, tempDir := h.DockerRunAndCopy(t,
						analyzeImage,
						"/layers",
						h.WithFlags(
							"--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock",
							"--env", "CNB_REGISTRY_AUTH={}", // In practice, we never set this variable in the daemon case. Setting to avoid failure to stat docker config directory when initializing cache.
						),
						h.WithArgs(
							analyzerPath,
							"-daemon",
							"-cache-image", cacheImage,
							"some-image",
						),
					)
					defer os.RemoveAll(tempDir)

					h.AssertPathDoesNotExist(t, filepath.Join(tempDir, "layers", "some-buildpack-id", "some-layer.sha"))
					h.AssertPathDoesNotExist(t, filepath.Join(tempDir, "layers", "some-buildpack-id", "some-layer.toml"))
				})
			})

			when("cache directory case", func() {
				it("uses the provided cache", func() {
					cacheVolume := h.SeedDockerVolume(t, cacheFixtureDir, daemonOS)
					defer h.DockerVolumeRemove(t, cacheVolume)

					_, tempDir := h.DockerRunAndCopy(t,
						analyzeImage,
						"/layers",
						h.WithFlags(
							"--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock",
							"--volume", fmt.Sprintf("%s:/cache", cacheVolume),
						),
						h.WithArgs(
							analyzerPath,
							"-daemon",
							"-cache-dir", "/cache",
							"some-image",
						),
					)
					defer os.RemoveAll(tempDir)

					h.AssertPathExists(t, filepath.Join(tempDir, "layers", "some-buildpack-id", "some-layer.sha"))
					h.AssertPathExists(t, filepath.Join(tempDir, "layers", "some-buildpack-id", "some-layer.toml"))
				})

				when("the provided cache directory isn't writeable by the CNB user's group", func() {
					it("recursively chowns the directory", func() {
						cacheVolume := h.SeedDockerVolume(t, cacheFixtureDir, daemonOS)
						defer h.DockerVolumeRemove(t, cacheVolume)

						output := h.DockerRun(t,
							analyzeImage,
							h.WithFlags(
								"--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock",
								"--volume", fmt.Sprintf("%s:/cache", cacheVolume),
							),
							h.WithBash(
								fmt.Sprintf("chown -R 9999:9999 /cache; chmod -R 775 /cache; %s -daemon -cache-dir /cache some-image; ls -alR /cache", analyzerPath),
							),
						)

						h.AssertMatch(t, output, "2222 3333 .+ \\.")
						h.AssertMatch(t, output, "2222 3333 .+ committed")
						h.AssertMatch(t, output, "2222 3333 .+ staging")
					})
				})

				when("the provided cache directory is writeable by the CNB user's group", func() {
					it("doesn't chown the directory", func() {
						cacheVolume := h.SeedDockerVolume(t, cacheFixtureDir, daemonOS)
						defer h.DockerVolumeRemove(t, cacheVolume)

						output := h.DockerRun(t,
							analyzeImage,
							h.WithFlags(
								"--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock",
								"--volume", fmt.Sprintf("%s:/cache", cacheVolume),
							),
							h.WithBash(
								fmt.Sprintf("chown -R 9999:3333 /cache; chmod -R 775 /cache; %s -daemon -cache-dir /cache some-image; ls -alR /cache", analyzerPath),
							),
						)

						h.AssertMatch(t, output, "9999 3333 .+ \\.")
						h.AssertMatch(t, output, "9999 3333 .+ committed")
						h.AssertMatch(t, output, "2222 3333 .+ staging")
					})
				})
			})
		})
	})

	when("registry case", func() {
		var appImage, appAuthConfig string

		it.Before(func() {
			metadata := flattenMetadata(t, filepath.Join("testdata", "analyzer", "app_image_metadata.json"), lifecycle.LayersMetadata{})
			appImage, appAuthConfig = buildRegistryImage(
				t,
				"some-app-image-"+h.RandString(10),
				filepath.Join("testdata", "analyzer", "app-image"),
				"--build-arg", fmt.Sprintf("metadata=%s", metadata),
			)
		})

		it.After(func() {
			h.DockerImageRemove(t, appImage)
		})

		it("writes analyzed.toml", func() {
			_, tempDir := h.DockerRunAndCopy(t,
				analyzeImage,
				"/layers/analyzed.toml",
				h.WithFlags("--env", "CNB_REGISTRY_AUTH={}"),
				h.WithArgs(analyzerPath, "some-image"),
			)
			defer os.RemoveAll(tempDir)

			assertAnalyzedMetadata(t, filepath.Join(tempDir, "analyzed.toml"))
		})

		when("app image is found", func() {
			it("restores layer metadata", func() {
				_, tempDir := h.DockerRunAndCopy(t,
					analyzeImage,
					"/layers",
					h.WithFlags(
						"--network", "host",
						"--env", fmt.Sprintf("CNB_REGISTRY_AUTH=%s", appAuthConfig),
					),
					h.WithArgs(analyzerPath, appImage),
				)
				defer os.RemoveAll(tempDir)

				layerFilenames := []string{
					"launch-build-cache-layer.sha",
					"launch-build-cache-layer.toml",
					"launch-cache-layer.sha",
					"launch-cache-layer.toml",
					"launch-layer.sha",
					"launch-layer.toml",
					"store.toml",
				}
				for _, filename := range layerFilenames {
					h.AssertPathExists(t, filepath.Join(tempDir, "layers", "some-buildpack-id", filename))
				}
			})

			when("skip layers is provided", func() {
				it("writes analyzed.toml and does not write buildpack layer metadata", func() {
					_, tempDir := h.DockerRunAndCopy(t,
						analyzeImage,
						"/layers",
						h.WithFlags(
							"--network", "host",
							"--env", fmt.Sprintf("CNB_REGISTRY_AUTH=%s", appAuthConfig),
						),
						h.WithArgs(
							analyzerPath,
							"-skip-layers",
							appImage,
						),
					)
					defer os.RemoveAll(tempDir)

					assertAnalyzedMetadata(t, filepath.Join(tempDir, "layers", "analyzed.toml"))
					h.AssertPathExists(t, filepath.Join(tempDir, "layers", "some-buildpack-id", "store.toml"))
					layerFilenames := []string{
						"launch-build-cache-layer.sha",
						"launch-build-cache-layer.toml",
						"launch-cache-layer.sha",
						"launch-cache-layer.toml",
						"launch-layer.sha",
						"launch-layer.toml",
					}
					for _, filename := range layerFilenames {
						h.AssertPathDoesNotExist(t, filepath.Join(tempDir, "layers", "some-buildpack-id", filename))
					}
				})
			})
		})

		when("CNB_REGISTRY_AUTH is not provided", func() {
			when("DOCKER_CONFIG is set", func() {
				it("succeeds", func() {
					// Copy docker config directory to a temp directory to avoid tampering with permissions for other tests.
					dockerConfig, err := ioutil.TempDir("", "test-docker-config-")
					defer os.RemoveAll(dockerConfig)
					h.AssertNil(t, err)
					h.RecursiveCopy(t, registry.DockerDirectory, dockerConfig)
					// Some OSes (like macOS) use symlinks for the standard temp dir.
					// Resolve it so it can be properly mounted by the Docker daemon.
					dockerConfig, err = filepath.EvalSymlinks(dockerConfig)
					h.AssertNil(t, err)

					_, tempDir := h.DockerRunAndCopy(t,
						analyzeImage,
						"/layers",
						h.WithFlags(
							"--mount", fmt.Sprintf("type=bind,source=%s,target=/mounted-docker-config", dockerConfig),
							"--env", fmt.Sprintf("DOCKER_CONFIG=%s", "/mounted-docker-config"),
							"--network", "host",
						),
						h.WithBash(
							fmt.Sprintf("chown -R 2222:3333 /mounted-docker-config;%s %s; ls -alR /layers", analyzerPath, appImage), // provide a real app image, so that we can test that the registry is accessible
						),
					)
					defer os.RemoveAll(tempDir)

					h.AssertPathExists(t, filepath.Join(tempDir, "layers", "some-buildpack-id"))
				})
			})
		})

		when("cache is provided", func() {
			when("cache image case", func() {
				var cacheImage, cacheAuthConfig string

				it.Before(func() {
					metadata := flattenMetadata(t, filepath.Join("testdata", "analyzer", "cache_image_metadata.json"), lifecycle.CacheMetadata{})
					cacheImage, cacheAuthConfig = buildRegistryImage(
						t,
						"some-cache-image-"+h.RandString(10),
						filepath.Join("testdata", "analyzer", "cache-image"),
						"--build-arg", fmt.Sprintf("metadata=%s", metadata),
					)
				})

				it.After(func() {
					h.DockerImageRemove(t, cacheImage)
				})

				it("uses the provided cache", func() {
					_, tempDir := h.DockerRunAndCopy(t,
						analyzeImage,
						"/layers",
						h.WithFlags(
							"--network", "host",
							"--env", fmt.Sprintf("CNB_REGISTRY_AUTH=%s", cacheAuthConfig),
						),
						h.WithArgs(
							analyzerPath,
							"-cache-image", cacheImage,
							"some-image",
						),
					)
					defer os.RemoveAll(tempDir)

					h.AssertPathExists(t, filepath.Join(tempDir, "layers", "some-buildpack-id", "some-layer.sha"))
					h.AssertPathExists(t, filepath.Join(tempDir, "layers", "some-buildpack-id", "some-layer.toml"))
				})
			})

			when("cache directory case", func() {
				it("uses the provided cache", func() {
					cacheVolume := h.SeedDockerVolume(t, cacheFixtureDir, daemonOS)
					defer h.DockerVolumeRemove(t, cacheVolume)

					_, tempDir := h.DockerRunAndCopy(t,
						analyzeImage,
						"/layers",
						h.WithFlags(
							"--env", "CNB_REGISTRY_AUTH={}",
							"--volume", fmt.Sprintf("%s:/cache", cacheVolume),
						),
						h.WithArgs(
							analyzerPath,
							"-cache-dir", "/cache",
							"some-image",
						),
					)
					defer os.RemoveAll(tempDir)

					h.AssertPathExists(t, filepath.Join(tempDir, "layers", "some-buildpack-id", "some-layer.sha"))
					h.AssertPathExists(t, filepath.Join(tempDir, "layers", "some-buildpack-id", "some-layer.toml"))
				})
			})
		})
	})
}

func flattenMetadata(t *testing.T, path string, metadataStruct interface{}) string {
	metadata, err := ioutil.ReadFile(path)
	h.AssertNil(t, err)

	// Unmarshal and marshal to strip unnecessary whitespace
	h.AssertNil(t, json.Unmarshal(metadata, &metadataStruct))
	flatMetadata, err := json.Marshal(metadataStruct)
	h.AssertNil(t, err)

	return string(flatMetadata)
}

func buildRegistryImage(t *testing.T, repoName, context string, buildArgs ...string) (string, string) {
	regRepoName := registry.RepoName(repoName)

	// Setup cmd
	cmdArgs := []string{
		"build",
		"-t", regRepoName,
	}
	cmdArgs = append(cmdArgs, buildArgs...)
	cmdArgs = append(cmdArgs, context)

	// Build image
	h.Run(t, exec.Command("docker", cmdArgs...))

	// Push image
	h.AssertNil(t, h.PushImage(h.DockerCli(t), regRepoName, registry.EncodedLabeledAuth()))

	// Setup auth
	authConfig, err := auth.BuildEnvVar(authn.DefaultKeychain, regRepoName)
	h.AssertNil(t, err)

	return regRepoName, authConfig
}

func assertAnalyzedMetadata(t *testing.T, path string) {
	contents, _ := ioutil.ReadFile(path)
	h.AssertEq(t, len(contents) > 0, true)

	var analyzedMd lifecycle.AnalyzedMetadata
	_, err := toml.Decode(string(contents), &analyzedMd)
	h.AssertNil(t, err)
}
