package maven

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const helpPluginVersion = "3.5.2"
const dependencyPluginVersion = "3.8.1"

// EffectiveModel is Maven's resolved model for the selected module. The Maven
// CLI is the authority for inheritance, active profiles and property
// interpolation; this Go package only decodes fields used by the language and
// debug servers.
type EffectiveModel struct {
	Module       Module            `json:"module"`
	Modules      []string          `json:"modules,omitempty"`
	Dependencies []Dependency      `json:"dependencies"`
	Properties   map[string]string `json:"properties"`
	Effective    bool              `json:"effective"`
	Warnings     []string          `json:"warnings,omitempty"`
}

// ResolveEffectiveModel asks Maven to materialize the POM that is active for
// this invocation. MAVEN_SETTINGS, MAVEN_PROFILES, MAVEN_OFFLINE, and
// MAVEN_USER_PROPERTIES are applied consistently with dependency resolution.
func ResolveEffectiveModel(ctx context.Context, root, moduleDir string) (EffectiveModel, error) {
	root, moduleDir, err := normalizeProjectPaths(root, moduleDir)
	if err != nil {
		return EffectiveModel{}, err
	}
	output, err := os.CreateTemp("", "jdtls-go-effective-*.xml")
	if err != nil {
		return EffectiveModel{}, err
	}
	outputPath := output.Name()
	if err := output.Close(); err != nil {
		_ = os.Remove(outputPath)
		return EffectiveModel{}, err
	}
	defer os.Remove(outputPath)

	args := []string{"-f", filepath.Join(moduleDir, "pom.xml")}
	args = append(args, mavenInvocationArgs(root)...)
	args = append(args,
		"-Doutput="+outputPath,
		"org.apache.maven.plugins:maven-help-plugin:"+helpPluginVersion+":effective-pom",
	)
	if _, err := runMaven(ctx, root, moduleDir, args...); err != nil {
		return EffectiveModel{}, fmt.Errorf("resolve Maven effective POM: %w", err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		return EffectiveModel{}, fmt.Errorf("Maven did not write the effective POM: %w", err)
	}
	var pom POM
	if err := xml.Unmarshal(data, &pom); err != nil {
		return EffectiveModel{}, fmt.Errorf("parse Maven effective POM: %w", err)
	}
	module := moduleFromPOM(moduleDir, filepath.Join(moduleDir, "pom.xml"), pom)
	properties := make(map[string]string, len(pom.Properties))
	for _, property := range pom.Properties {
		properties[property.Name] = property.Value
	}
	return EffectiveModel{Module: module, Modules: pom.Modules, Dependencies: pom.Dependencies, Properties: properties, Effective: true}, nil
}

// DiscoverEffective follows module declarations from each effective model, so
// modules contributed by active profiles are included in the workspace index.
func DiscoverEffective(ctx context.Context, root string) ([]Module, map[string]EffectiveModel, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	var modules []Module
	models := make(map[string]EffectiveModel)
	seen := make(map[string]bool)
	var walk func(string) error
	walk = func(dir string) error {
		dir, err = filepath.Abs(dir)
		if err != nil {
			return err
		}
		if seen[dir] {
			return nil
		}
		seen[dir] = true
		model, err := ResolveEffectiveModel(ctx, root, dir)
		if err != nil {
			return err
		}
		models[dir] = model
		modules = append(modules, model.Module)
		for _, child := range model.Modules {
			childDir := child
			if !filepath.IsAbs(childDir) {
				childDir = filepath.Join(dir, childDir)
			}
			if err := walk(filepath.Clean(childDir)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, nil, err
	}
	return modules, models, nil
}

// ResolveSourceAttachments resolves source artifacts for dependencies in the
// selected Maven scope and maps each binary classpath archive to an adjacent
// source JAR when Maven has one. Project modules are represented by source roots
// in EffectiveModel rather than by synthetic source JARs.
func ResolveSourceAttachments(ctx context.Context, root, moduleDir, scope string) (map[string]string, error) {
	root, moduleDir, err := normalizeProjectPaths(root, moduleDir)
	if err != nil {
		return nil, err
	}
	if scope == "" {
		scope = "compile"
	}
	if scope != "compile" && scope != "runtime" && scope != "test" {
		return nil, fmt.Errorf("unsupported Maven source scope %q", scope)
	}
	args := []string{"-f", filepath.Join(moduleDir, "pom.xml")}
	args = append(args, mavenInvocationArgs(root)...)
	args = append(args,
		"-DincludeScope="+scope,
		"org.apache.maven.plugins:maven-dependency-plugin:"+dependencyPluginVersion+":resolve-sources",
	)
	if _, err := runMaven(ctx, root, moduleDir, args...); err != nil {
		return nil, fmt.Errorf("resolve Maven source attachments: %w", err)
	}

	classpath, err := resolveClasspathWithPaths(ctx, root, moduleDir, scope)
	if err != nil {
		return nil, err
	}
	attachments := make(map[string]string)
	for _, entry := range classpath.Entries {
		if filepath.Ext(entry) != ".jar" {
			continue
		}
		if source := adjacentSourceArchive(entry); source != "" {
			attachments[entry] = source
		}
	}
	return attachments, nil
}

func adjacentSourceArchive(binary string) string {
	base := filepath.Base(binary)
	if strings.HasSuffix(base, "-sources.jar") || !strings.HasSuffix(base, ".jar") {
		return ""
	}
	stem := strings.TrimSuffix(base, ".jar")
	var candidates []string
	if strings.HasSuffix(stem, "-tests") {
		candidates = append(candidates, strings.TrimSuffix(stem, "-tests")+"-test-sources.jar")
	}
	candidates = append(candidates, stem+"-sources.jar")
	for _, candidate := range candidates {
		path := filepath.Join(filepath.Dir(binary), candidate)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	return ""
}

func normalizeProjectPaths(root, moduleDir string) (string, string, error) {
	if root == "" {
		return "", "", errors.New("Maven project root is required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	if moduleDir == "" || moduleDir == "." {
		moduleDir = root
	} else if !filepath.IsAbs(moduleDir) {
		moduleDir = filepath.Join(root, moduleDir)
	}
	moduleDir, err = filepath.Abs(moduleDir)
	if err != nil {
		return "", "", err
	}
	if _, err := os.Stat(filepath.Join(moduleDir, "pom.xml")); err != nil {
		return "", "", fmt.Errorf("no Maven pom.xml in module %s: %w", moduleDir, err)
	}
	return root, moduleDir, nil
}

func mavenInvocationArgs(root string) []string {
	var args []string
	settings := os.Getenv("MAVEN_SETTINGS")
	if settings == "" {
		// OpenIDEA language servers do not necessarily inherit shell variables.
		// Honor a settings.xml checked into an ancestor of the workspace before
		// falling back to Maven's per-user defaults.
		if absoluteRoot, err := filepath.Abs(root); err == nil {
			for dir := absoluteRoot; ; dir = filepath.Dir(dir) {
				candidate := filepath.Join(dir, "settings.xml")
				if info, statErr := os.Stat(candidate); statErr == nil && info.Mode().IsRegular() {
					settings = candidate
					break
				}
				parent := filepath.Dir(dir)
				if parent == dir {
					break
				}
			}
		}
	}
	if settings != "" {
		args = append(args, "-s", settings)
	}
	if profiles := strings.TrimSpace(os.Getenv("MAVEN_PROFILES")); profiles != "" {
		args = append(args, "-P", profiles)
	}
	if os.Getenv("MAVEN_OFFLINE") == "true" || os.Getenv("MAVEN_OFFLINE") == "1" {
		args = append(args, "-o")
	}
	for _, property := range strings.Fields(os.Getenv("MAVEN_USER_PROPERTIES")) {
		property = strings.TrimSpace(property)
		if property == "" {
			continue
		}
		if !strings.HasPrefix(property, "-D") {
			property = "-D" + property
		}
		args = append(args, property)
	}
	return args
}

func runMaven(ctx context.Context, root, moduleDir string, args ...string) (string, error) {
	program, baseArgs, err := mavenCommand(root)
	if err != nil {
		return "", err
	}
	commandArgs := append(append([]string{}, baseArgs...), args...)
	cmd := exec.CommandContext(ctx, program, commandArgs...)
	cmd.Dir = moduleDir
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

// BuildWorkspace runs Maven's compile goal for the workspace root. JDT LS
// exposes this as java/buildWorkspace before resolving launch classpaths.
func BuildWorkspace(ctx context.Context, root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	args := []string{"-f", filepath.Join(root, "pom.xml")}
	args = append(args, mavenInvocationArgs(root)...)
	args = append(args, "-DskipTests", "compile")
	_, err = runMaven(ctx, root, root, args...)
	if err != nil {
		return fmt.Errorf("Maven workspace build failed: %w", err)
	}
	return nil
}

// ArchiveContents returns normalized Java source entries from a source JAR.
// It is kept here to let LSP source lookup share the same archive rules as
// Maven attachment resolution.
func ArchiveContents(path string) (map[string][]byte, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	contents := make(map[string][]byte)
	for _, file := range archive.File {
		if file.FileInfo().IsDir() || filepath.Ext(file.Name) != ".java" {
			continue
		}
		input, err := file.Open()
		if err != nil {
			continue
		}
		data, readErr := io.ReadAll(input)
		_ = input.Close()
		if readErr == nil {
			contents[filepath.ToSlash(file.Name)] = data
		}
	}
	return contents, nil
}

func ReadArchiveFile(path, entry string) ([]byte, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	entry = filepath.ToSlash(filepath.Clean(filepath.FromSlash(entry)))
	for _, file := range archive.File {
		if filepath.ToSlash(file.Name) != entry {
			continue
		}
		input, err := file.Open()
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(input)
		_ = input.Close()
		return data, readErr
	}
	return nil, fmt.Errorf("%s not found in source archive %s", entry, path)
}

func isWindows() bool { return runtime.GOOS == "windows" }
