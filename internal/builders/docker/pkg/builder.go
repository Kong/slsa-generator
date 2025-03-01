// Copyright 2022 SLSA Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pkg

// This file contains the structs and functionality for building artifacts
// using a builder Docker image, and user-provided build configurations.
//
// In particular, this file defines a GitClient struct for handling git
// commands for fetching the repo at a Git commit hash. It also defines an
// exposed Builder struct for handling the steps of building artifacts using a
// Docker image.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"

	intoto "github.com/in-toto/in-toto-golang/in_toto"
	slsa1 "github.com/in-toto/in-toto-golang/in_toto/slsa_provenance/v1"

	"github.com/Kong/slsa-github-generator/internal/utils"
)

var (
	// errGitCommitMismatch indicates that the repo is checked out at an unexpected commit hash.
	errGitCommitMismatch = errors.New("commit mismatch")

	// errGitFetch indicates an error when cloning a Git repo.
	errGitFetch = errors.New("git fetch")

	// errGitCheckout indicates an error when checking out a given commit hash.
	errGitCheckout = errors.New("git checkout")
)

// DockerBuild represents a state in the process of building the artifacts
// where the source repository is checked out and the config file is loaded and
// parsed, and we are ready for running the `docker run` command.
type DockerBuild struct {
	config      *DockerBuildConfig
	buildConfig *BuildConfig
	RepoInfo    *RepoCheckoutInfo
}

// RepoCheckoutInfo contains info about the location of a locally checked out
// repository.
type RepoCheckoutInfo struct {
	// Path to the root of the repo.
	RepoRoot string
}

// Fetcher is an interface with a single method Fetch, for fetching a
// repository from its source.
type Fetcher interface {
	Fetch() (*RepoCheckoutInfo, error)
}

// Builder is responsible for setting up the environment and using docker
// commands to build artifacts as specified in a DockerBuildConfig.
type Builder struct {
	repoFetcher Fetcher
	config      DockerBuildConfig
}

// NewBuilderWithGitFetcher creates a new Builder that fetches the sources
// from a Git repository.
func NewBuilderWithGitFetcher(config *DockerBuildConfig) (*Builder, error) {
	gc, err := newGitClient(config, 0 /* depth */)
	if err != nil {
		return nil, fmt.Errorf("could not create builder: %v", err)
	}

	return &Builder{
		repoFetcher: gc,
		config:      *config,
	}, nil
}

// CreateBuildDefinition creates a BuildDefinition from the DockerBuildConfig
// and BuildConfig in this DockerBuild.
func (db *DockerBuild) CreateBuildDefinition() *slsa1.ProvenanceBuildDefinition {
	ep := ContainerBasedExternalParameters{
		Source:       sourceArtifact(db.config),
		BuilderImage: builderImage(db.config),
		ConfigPath:   db.config.BuildConfigPath,
		Config:       *db.buildConfig,
	}

	// Currently we don't have any SystemParameters, so this fields is left empty.
	return &slsa1.ProvenanceBuildDefinition{
		BuildType:          ContainerBasedBuildType,
		ExternalParameters: ep,
		// The source repository is also added as a resolved dependency.
		ResolvedDependencies: []slsa1.ResourceDescriptor{sourceArtifact(db.config)},
	}
}

// sourceArtifact returns the source repo and its digest as an instance of ResourceDescriptor.
func sourceArtifact(config *DockerBuildConfig) slsa1.ResourceDescriptor {
	return slsa1.ResourceDescriptor{
		URI:    config.SourceRepo,
		Digest: config.SourceDigest.ToMap(),
	}
}

// builderImage returns the builder image as an instance of ResourceDescriptor.
func builderImage(config *DockerBuildConfig) slsa1.ResourceDescriptor {
	return slsa1.ResourceDescriptor{
		URI:    config.BuilderImage.ToString(),
		Digest: config.BuilderImage.Digest.ToMap(),
	}
}

// SetUpBuildState sets up the build by checking out the source repository and
// loading the config file. It returns an instance of DockerBuild, or an error
// if setting up the build state fails.
func (b *Builder) SetUpBuildState() (*DockerBuild, error) {
	// 1. Check out the repo, or verify that it is checked out.
	repoInfo, err := b.repoFetcher.Fetch()
	if err != nil {
		return nil, fmt.Errorf("couldn't verify or fetch source repo: %v", err)
	}

	// 2. Load and parse the config file.
	bc, err := b.config.LoadBuildConfigFromFile()
	if err != nil {
		return nil, fmt.Errorf("couldn't load config file from %q: %v", b.config.BuildConfigPath, err)
	}

	// 3. Check that the ArtifactPath pattern does not match any existing files,
	// so that we don't accidentally generate provenances for the wrong files.
	if err := CheckExistingFiles(bc.ArtifactPath); err != nil {
		return nil, err
	}

	db := &DockerBuild{
		config:      &b.config,
		buildConfig: bc,
		RepoInfo:    repoInfo,
	}
	return db, nil
}

// BuildArtifacts builds the artifacts based on the user-provided inputs, and
// returns the names and SHA256 digests of the generated artifacts.
func (db *DockerBuild) BuildArtifacts(outputFolder string) ([]intoto.Subject, error) {
	if err := runDockerRun(db); err != nil {
		return nil, fmt.Errorf("running `docker run` failed: %v", err)
	}
	return inspectAndWriteArtifacts(db.buildConfig.ArtifactPath, outputFolder, db.RepoInfo.RepoRoot)
}

func runDockerRun(db *DockerBuild) error {
	// Get the current working directory. We will mount it as a Docker volume.
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("couldn't get the current working directory: %v", err)
	}

	defaultDockerRunFlags := []string{
		// Mount the current working directory to workspace.
		fmt.Sprintf("--volume=%s:/workspace", cwd),
		"--workdir=/workspace",
		// Remove the container file system after the container exits.
		"--rm",
	}

	buildDef := db.CreateBuildDefinition()
	containerEp, ok := buildDef.ExternalParameters.(ContainerBasedExternalParameters)
	if !ok {
		return fmt.Errorf("expected container-based external parameters")
	}

	var args []string
	args = append(args, "run")
	args = append(args, defaultDockerRunFlags...)
	args = append(args, containerEp.BuilderImage.URI)
	args = append(args, db.buildConfig.Command...)
	cmd := exec.Command("docker", args...)

	log.Printf("Running command: %q.", cmd.String())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("couldn't get the command's stdout: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("couldn't get the command's stderr: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("couldn't start the 'git checkout' command: %v", err)
	}

	files, err := saveToTempFile(db.config.Verbose, stdout, stderr)
	if err != nil {
		return fmt.Errorf("cannot save logs and errs to file: %v", err)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("failed to complete the command: %v; see %s for logs, and %s for errors",
			err, files[0], files[1])
	}

	return nil
}

// GitClient provides data and functions for fetching the source files from a
// Git repository.
type GitClient struct {
	sourceRepo    *string
	sourceRef     *string
	sourceDigest  *Digest
	checkoutInfo  *RepoCheckoutInfo
	logFiles      []string
	errFiles      []string
	forceCheckout bool
	verbose       bool
	depth         int
}

func newGitClient(config *DockerBuildConfig, depth int) (*GitClient, error) {
	repo := config.SourceRepo
	parsed, err := url.Parse(repo)
	if err != nil {
		return nil, fmt.Errorf("could not parse repo URI: %v", err)
	}

	switch parsed.Scheme {
	case "https":
		break
	case "git+https":
		repo = strings.Replace(repo, "git+https", "https", 1)
	case "https+git":
		repo = strings.Replace(repo, "https+git", "https", 1)
	default:
		return nil, fmt.Errorf("unsupported scheme: %v", parsed.Scheme)
	}

	// Retrieve the ref if a tag is added to the repository.
	var sourceRef *string
	refParts := strings.Split(repo, "@")
	switch len(refParts) {
	case 2:
		// A source reference was provided.
		sourceRef = &refParts[1]
		repo = strings.TrimSuffix(repo, "@"+refParts[1])
	case 1:
		// No source reference was provided.
	default:
		return nil, fmt.Errorf("invalid source repository format: %s", repo)
	}

	return &GitClient{
		sourceRepo:    &repo,
		sourceRef:     sourceRef,
		sourceDigest:  &config.SourceDigest,
		forceCheckout: config.ForceCheckout,
		depth:         depth,
		checkoutInfo:  &RepoCheckoutInfo{},
		verbose:       config.Verbose,
	}, nil
}

func (c *GitClient) cleanupAllFiles() {
	c.checkoutInfo.Cleanup()
	for _, file := range append(c.logFiles, c.errFiles...) {
		if err := os.Remove(file); err != nil {
			log.Printf("failed to remove temp file %q: %v", file, err)
		}
	}
}

// Fetch is implemented for GitClient to make it usable in contexts where a
// Fetcher is needed.
func (c *GitClient) Fetch() (*RepoCheckoutInfo, error) {
	if err := c.verifyOrFetchRepo(); err != nil {
		return nil, err
	}
	return c.checkoutInfo, nil
}

// verifyOrFetchRepo checks that the current working directly is a Git repository
// at the expected Git commit hash; fetches the repo, if this is not the case.
func (c *GitClient) verifyOrFetchRepo() error {
	if c.sourceDigest.Alg != "sha1" {
		return fmt.Errorf("git commit digest must be a sha1 digest")
	}
	repoIsCheckedOut, err := c.verifyRefAndCommit()
	if err != nil && !c.forceCheckout {
		return err
	}
	if !repoIsCheckedOut || c.forceCheckout {
		if err := c.fetchSourcesFromGitRepo(); err != nil {
			return fmt.Errorf("couldn't fetch sources from %q at commit %q: %v", *c.sourceRepo, c.sourceDigest, err)
		}
	}
	return nil
}

// verifyRefAndCommit checks that the current working directory is the root of a Git
// repository at the given commit hash. If a source ref is also specified, verifies
// that the ref resolves to the given commit hash.
// Returns an error if the working directory is a Git repository at a different commit
// or ref.
func (c *GitClient) verifyRefAndCommit() (bool, error) {
	checkCmds := []*exec.Cmd{exec.Command("git", "rev-parse", "--verify", "HEAD")}
	if c.sourceRef != nil {
		sourceRef := *c.sourceRef
		checkCmds = append(checkCmds, exec.Command("git", "show-ref", "--hash", "--verify", sourceRef))
	}

	for _, cmd := range checkCmds {
		lastCommitIDBytes, err := cmd.Output()
		if err != nil {
			// The current working directory is not a git repo.
			return false, nil
		}
		lastCommitID := strings.TrimSpace(string(lastCommitIDBytes))

		if lastCommitID != c.sourceDigest.Value {
			return false, fmt.Errorf(
				"%w: the repo is already checked out at a different commit (%q)",
				errGitCommitMismatch, lastCommitID)
		}
	}

	return true, nil
}

// fetchSourcesFromGitRepo clones a repo from the URL given in this GitClient,
// up to the depth given in this GitClient, into a temporary directory. It then
// checks out the specified commit. If depth is not a positive number, the
// entire repo and its history is cloned.
// Returns an error if the repo cannot be cloned, or the commit hash does not
// exist. Otherwise, updates this GitClient with RepoCheckoutInfo containing
// the absolute path of the root of the repo, and other generated files paths.
func (c *GitClient) fetchSourcesFromGitRepo() error {
	// create a temp folder in the current directory for fetching the repo.
	targetDir, err := os.MkdirTemp("", "release-*")
	if err != nil {
		return fmt.Errorf("couldn't create temp directory: %v", err)
	}
	log.Printf("Checking out the repo in %q.", targetDir)

	// Make targetDir and its parents, and cd to it.
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("couldn't create directories at %q: %v", targetDir, err)
	}
	if err := os.Chdir(targetDir); err != nil {
		return fmt.Errorf("couldn't change directory to %q: %v", targetDir, err)
	}

	// Clone the repo.
	if err = c.cloneGitRepo(); err != nil {
		return fmt.Errorf("%w: couldn't clone the Git repo: %w", errGitFetch, err)
	}

	// Change directory to the root of the cloned repo.
	repoName := path.Base(*c.sourceRepo)
	if err := os.Chdir(repoName); err != nil {
		return fmt.Errorf("couldn't change directory to %q: %v", repoName, err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("couldn't get current working directory: %v", err)
	}

	// Checkout the commit.
	if err = c.checkoutGitCommit(); err != nil {
		return fmt.Errorf("%w: couldn't checkout the Git commit: %w", errGitCheckout, err)
	}

	c.checkoutInfo.RepoRoot = cwd

	return nil
}

// Clones a Git repo from the URI in this GitClient, up to the depth given in
// this GitClient. If depth is 0 or negative, the entire repo is cloned.
func (c *GitClient) cloneGitRepo() error {
	//#nosec G204 -- Input from user config file.
	cmd := exec.Command("git", "clone", *c.sourceRepo)
	if c.depth > 0 {
		//#nosec G204 -- Input from user config file.
		cmd = exec.Command("git", "clone", "--depth", fmt.Sprintf("%d", c.depth), *c.sourceRepo)
	}
	log.Printf("Cloning the repo from %s...", *c.sourceRepo)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("couldn't get the command's stdout: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("couldn't get the command's stderr: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("couldn't start the 'git checkout' command: %v", err)
	}

	files, err := saveToTempFile(c.verbose, stdout, stderr)
	if err != nil {
		return fmt.Errorf("cannot save logs and errs to file: %v", err)
	}
	c.logFiles = append(c.logFiles, files[0])
	c.errFiles = append(c.errFiles, files[1])

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("failed to complete the command: %v; see %q for logs, and %q for errors",
			err, files[0], files[1])
	}
	log.Printf("'git clone' completed. See %q, and %q for logs, and errors.", files[0], files[1])

	return nil
}

func (c *GitClient) checkoutGitCommit() error {
	//#nosec G204 -- Input from user config file.
	cmd := exec.Command("git", "checkout", c.sourceDigest.Value)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("couldn't get the command's stdout: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("couldn't get the command's stderr: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("couldn't start the 'git checkout' command: %v", err)
	}

	files, err := saveToTempFile(c.verbose, stdout, stderr)
	if err != nil {
		return fmt.Errorf("cannot save logs and errs to file: %v", err)
	}
	c.logFiles = append(c.logFiles, files[0])
	c.errFiles = append(c.errFiles, files[1])

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("failed to complete the command: %v; see %q for logs, and %q for errors",
			err, files[0], files[1])
	}

	ok, err := c.verifyRefAndCommit()
	if err != nil || !ok {
		return fmt.Errorf("failed to verify ref and commit: %v", err)
	}

	return nil
}

type tempFileResult struct {
	File *os.File
	Err  error
}

// A helper function used by saveToTempFile to process one individual file.
// This should be called in a goroutine, and the channels passed in should be owned by the caller,
// and remain open until the goroutine completes.
func saveOneTempFile(verbose bool, reader io.Reader, fileChannel chan tempFileResult, printChannel chan string) {
	var allBytes []byte
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		bytes := scanner.Bytes()
		allBytes = append(allBytes, bytes...)
		allBytes = append(allBytes, '\n')

		if verbose {
			printChannel <- string(bytes)
		}
	}

	tmpfile, err := os.CreateTemp("", "log-*.txt")
	if err != nil {
		fileChannel <- tempFileResult{Err: err}
		return
	}
	defer tmpfile.Close()

	if _, err := tmpfile.Write(allBytes); err != nil {
		fileChannel <- tempFileResult{Err: fmt.Errorf("couldn't write bytes to tempfile: %v", err)}
	} else {
		fileChannel <- tempFileResult{File: tmpfile}
	}
}

// saveToTempFile creates a tempfile in `/tmp` and writes the content of the
// given readers to that file.
// It processes all provided readers concurrently.
func saveToTempFile(verbose bool, readers ...io.Reader) ([]string, error) {
	if verbose {
		fmt.Print("\n\n>>>>>>>>>>>>>> output from command <<<<<<<<<<<<<<\n")
	}
	var wg sync.WaitGroup
	// We need to make sure the fileChannel has enough buffere space to hold everything,
	// since it won't be processed until the very end.
	fileChannel := make(chan tempFileResult, len(readers))
	printChannel := make(chan string)

	// Start a goroutine to process each Reader concurrently.
	for _, reader := range readers {
		wg.Add(1)
		go func(reader io.Reader) {
			defer wg.Done()
			saveOneTempFile(verbose, reader, fileChannel, printChannel)
		}(reader)
	}

	// Close the channel once all goroutines have finished.
	// We do the waiting in a goroutine so that we can move on
	// to the lines below that handle printing the values passed on
	// the print channel.
	go func() {
		wg.Wait()
		close(printChannel)
		close(fileChannel)
	}()

	// Print the output as it comes in.
	// Once all of the provided readers have been closed, the WaitGroup
	// will move to the Done state, completing this loop and allowing us to
	// move to the filename collection below.
	for line := range printChannel {
		fmt.Println(line)
	}

	var files []string
	for result := range fileChannel {
		if result.Err != nil {
			return nil, result.Err
		}
		files = append(files, result.File.Name())
	}

	return files, nil
}

// CheckExistingFiles checks if any files match the given pattern, and returns an error if so.
func CheckExistingFiles(pattern string) error {
	matches, err := filepath.Glob(pattern)
	// The only possible error is ErrBadPattern.
	if err != nil {
		return fmt.Errorf("the pattern (%q) is malformed: %v", pattern, err)
	}

	if len(matches) == 0 {
		return nil
	}
	return fmt.Errorf("the specified pattern (%q) matches %d existing files; expected no matches", pattern, len(matches))
}

// Finds all files matching the given pattern, measures the SHA256 digest of
// each file, and returns filenames and digests as an array of intoto.Subject.
// This also writes the output to a configured output folder, if provided.
// Precondition: The pattern is a relative file path pattern.
func inspectAndWriteArtifacts(pattern, outputFolder, root string) ([]intoto.Subject, error) {
	matches, err := filepath.Glob(pattern)
	// The only possible error is ErrBadPattern.
	if err != nil {
		return nil, fmt.Errorf("the pattern (%q) is malformed: %v", pattern, err)
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("no files matching the pattern %q: %v", pattern, err)
	}

	var subjects []intoto.Subject
	for _, path := range matches {
		data, err := utils.SafeReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("couldn't read file %q: %v", path, err)
		}

		// Write intoto subjects to output
		subject, err := toIntotoSubject(data, path)
		if err != nil {
			return nil, err
		}
		subjects = append(subjects, *subject)

		if outputFolder != "" {
			// Write output file to output folder using the path relative to the root
			// of the source repository.
			if root != "" {
				path, err = filepath.Abs(path)
				if err != nil {
					return nil, err
				}
			}
			relPath, err := filepath.Rel(root, path)
			if err != nil {
				return nil, fmt.Errorf("Root %s Path %s: %w", root, path, err)
			}
			w, err := utils.CreateNewFileUnderDirectory(relPath, outputFolder, os.O_WRONLY)
			if err != nil {
				return nil, fmt.Errorf("creating new output file: %v", err)
			}
			if _, err := w.Write(data); err != nil {
				return nil, fmt.Errorf("writing output file: %v", err)
			}
		}
	}

	return subjects, nil
}

// Reads the file in the given path and returns its name and digest wrapped in
// an intoto.Subject.
func toIntotoSubject(data []byte, filePath string) (*intoto.Subject, error) {
	sum256 := sha256.Sum256(data)
	digest := hex.EncodeToString(sum256[:])
	name := filepath.Base(filePath)
	subject := &intoto.Subject{
		Name:   name,
		Digest: map[string]string{"sha256": digest},
	}
	return subject, nil
}

// Cleanup removes the generated temp files. But it might not be able to remove
// all the files, for instance the ones generated by the build script.
func (info *RepoCheckoutInfo) Cleanup() {
	// Some files are generated by the build toolchain (e.g., cargo), and cannot
	// be removed. We still want to remove all other files to avoid taking up
	// too much space, particularly when running locally.
	if info.RepoRoot == "" {
		return
	}
	if err := os.RemoveAll(info.RepoRoot); err != nil {
		log.Printf("failed to remove the temp files: %v", err)
	}
}

// ProvenanceStatementSLSA1 is a convenience class to facilitate parsing a
// JSON document to a SLSAv1 provenance object.
type ProvenanceStatementSLSA1 struct {
	intoto.StatementHeader
	Predicate slsa1.ProvenancePredicate `json:"predicate"`
}

// ParseProvenance parses a byte array into an instance of ProvenanceStatementSLSA1.
func ParseProvenance(bytes []byte) (*ProvenanceStatementSLSA1, error) {
	var statement ProvenanceStatementSLSA1
	if err := json.Unmarshal(bytes, &statement); err != nil {
		return nil, fmt.Errorf("could not unmarshal the provenance file:\n%v", err)
	}

	// ExternalParameters is an interface in slsa1.ProvenancePredicate, so we
	// marshal and unmarshal it as an instance of ContainerBasedExternalParameters
	// to be able to use the actual type.
	var ep ContainerBasedExternalParameters
	b, err := json.Marshal(statement.Predicate.BuildDefinition.ExternalParameters)
	if err != nil {
		return nil, fmt.Errorf("could not marshal map into JSON bytes: %v", err)
	}

	if err = json.Unmarshal(b, &ep); err != nil {
		return nil, fmt.Errorf("could not unmarshal JSON bytes into a BuildConfig: %v", err)
	}

	statement.Predicate.BuildDefinition.ExternalParameters = ep

	return &statement, nil
}

// ToDockerBuildConfig creates an instance of DockerBuildConfig using the
// external parameters in this provenance.
func (p *ProvenanceStatementSLSA1) ToDockerBuildConfig(forceCheckout bool) (*DockerBuildConfig, error) {
	ep, ok := p.Predicate.BuildDefinition.ExternalParameters.(ContainerBasedExternalParameters)
	if !ok {
		return nil, fmt.Errorf("failed to cast ExternalParameters to ContainerBasedExternalParameters")
	}

	di, err := validateDockerImage(ep.BuilderImage.URI)
	if err != nil {
		return nil, fmt.Errorf("validating Docker image URI: %v", err)
	}
	if di.Digest.Value != ep.BuilderImage.Digest[di.Digest.Alg] {
		return nil, fmt.Errorf("invalid Docker image digest")
	}

	val, ok := ep.Source.Digest["sha1"]
	if !ok {
		return nil, fmt.Errorf("missing sha1 digest for source")
	}
	sd := Digest{
		Alg:   "sha1",
		Value: val,
	}

	return &DockerBuildConfig{
		SourceRepo:      ep.Source.URI,
		SourceDigest:    sd,
		BuilderImage:    *di,
		BuildConfigPath: ep.ConfigPath,
		ForceCheckout:   forceCheckout,
		Verbose:         false,
	}, nil
}
