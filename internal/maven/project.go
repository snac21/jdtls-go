package maven

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type POM struct {
	XMLName      xml.Name     `xml:"project"`
	GroupID      string       `xml:"groupId"`
	Artifact     string       `xml:"artifactId"`
	Version      string       `xml:"version"`
	Modules      []string     `xml:"modules>module"`
	Build        Build        `xml:"build"`
	Properties   Properties   `xml:"properties"`
	Parent       Parent       `xml:"parent"`
	Dependencies []Dependency `xml:"dependencies>dependency"`
}

type Parent struct {
	GroupID string `xml:"groupId"`
	Version string `xml:"version"`
}

type Module struct {
	Dir                  string   `json:"dir"`
	POM                  string   `json:"pom"`
	GroupID              string   `json:"groupId,omitempty"`
	Artifact             string   `json:"artifactId,omitempty"`
	Version              string   `json:"version,omitempty"`
	SourceRoots          []string `json:"sourceRoots"`
	TestSourceRoots      []string `json:"testSourceRoots"`
	ResourceRoots        []string `json:"resourceRoots"`
	TestResourceRoots    []string `json:"testResourceRoots"`
	OutputDirectory      string   `json:"outputDirectory"`
	TestOutputDirectory  string   `json:"testOutputDirectory"`
	CompilerRelease      string   `json:"compilerRelease,omitempty"`
	CompilerSource       string   `json:"compilerSource,omitempty"`
	CompilerTarget       string   `json:"compilerTarget,omitempty"`
	GeneratedSourceRoots []string `json:"generatedSourceRoots,omitempty"`
}

type Build struct {
	SourceDirectory     string     `xml:"sourceDirectory"`
	TestSourceDirectory string     `xml:"testSourceDirectory"`
	OutputDirectory     string     `xml:"outputDirectory"`
	TestOutputDirectory string     `xml:"testOutputDirectory"`
	Resources           []Resource `xml:"resources>resource"`
	TestResources       []Resource `xml:"testResources>testResource"`
	Plugins             []Plugin   `xml:"plugins>plugin"`
}

type Plugin struct {
	GroupID       string                `xml:"groupId"`
	ArtifactID    string                `xml:"artifactId"`
	Configuration CompilerConfiguration `xml:"configuration"`
}

type CompilerConfiguration struct {
	Release                   string   `xml:"release"`
	Source                    string   `xml:"source"`
	Target                    string   `xml:"target"`
	GeneratedSourcesDirectory string   `xml:"generatedSourcesDirectory"`
	Sources                   []string `xml:"sources>source"`
}

type Dependency struct {
	GroupID    string `xml:"groupId" json:"groupId"`
	ArtifactID string `xml:"artifactId" json:"artifactId"`
	Version    string `xml:"version" json:"version"`
	Type       string `xml:"type" json:"type"`
	Classifier string `xml:"classifier" json:"classifier,omitempty"`
	Scope      string `xml:"scope" json:"scope"`
	Optional   bool   `xml:"optional" json:"optional"`
}

type Resource struct {
	Directory string `xml:"directory"`
}
type Property struct {
	Name  string `xml:"name,attr"`
	Value string `xml:",chardata"`
}
type Properties []Property

func (properties *Properties) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case xml.StartElement:
			var content string
			if err := decoder.DecodeElement(&content, &value); err != nil {
				return err
			}
			*properties = append(*properties, Property{Name: value.Name.Local, Value: strings.TrimSpace(content)})
		case xml.EndElement:
			if value.Name == start.Name {
				return nil
			}
		}
	}
}

type Classpath struct {
	Module    string            `json:"module"`
	Entries   []string          `json:"entries"`
	Separator string            `json:"separator"`
	Sources   map[string]string `json:"sources,omitempty"`
	Warnings  []string          `json:"warnings,omitempty"`
}

// Discover walks only Maven reactor module declarations. It never inspects Gradle files.
func Discover(root string) ([]Module, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var modules []Module
	seen := map[string]bool{}
	var walk func(string) error
	walk = func(dir string) error {
		pomPath := filepath.Join(dir, "pom.xml")
		if seen[pomPath] {
			return nil
		}
		seen[pomPath] = true
		data, err := os.ReadFile(pomPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", pomPath, err)
		}
		var pom POM
		if err := xml.Unmarshal(data, &pom); err != nil {
			return fmt.Errorf("parse %s: %w", pomPath, err)
		}
		modules = append(modules, moduleFromPOM(dir, pomPath, pom))
		for _, child := range pom.Modules {
			childDir := filepath.Clean(filepath.Join(dir, strings.TrimSpace(child)))
			if err := walk(childDir); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return modules, nil
}

func moduleFromPOM(dir, pomPath string, pom POM) Module {
	properties := map[string]string{}
	for _, property := range pom.Properties {
		properties[property.Name] = property.Value
	}
	resolve := func(value, fallback string) string {
		if value == "" {
			value = fallback
		}
		value = strings.ReplaceAll(value, "${project.basedir}", dir)
		value = strings.ReplaceAll(value, "${basedir}", dir)
		for key, replacement := range properties {
			value = strings.ReplaceAll(value, "${"+key+"}", replacement)
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(dir, value)
		}
		return filepath.Clean(value)
	}
	groupID, version := pom.GroupID, pom.Version
	if groupID == "" {
		groupID = pom.Parent.GroupID
	}
	if version == "" {
		version = pom.Parent.Version
	}
	module := Module{Dir: dir, POM: pomPath, GroupID: groupID, Artifact: pom.Artifact, Version: version}
	for _, property := range pom.Properties {
		switch property.Name {
		case "maven.compiler.release":
			module.CompilerRelease = property.Value
		case "maven.compiler.source":
			module.CompilerSource = property.Value
		case "maven.compiler.target":
			module.CompilerTarget = property.Value
		}
	}
	for _, plugin := range pom.Build.Plugins {
		if generated := plugin.Configuration.GeneratedSourcesDirectory; generated != "" {
			module.GeneratedSourceRoots = append(module.GeneratedSourceRoots, resolve(generated, ""))
		}
		for _, source := range plugin.Configuration.Sources {
			if source != "" {
				module.GeneratedSourceRoots = append(module.GeneratedSourceRoots, resolve(source, ""))
			}
		}
		if plugin.ArtifactID != "maven-compiler-plugin" {
			continue
		}
		if plugin.Configuration.Release != "" {
			module.CompilerRelease = plugin.Configuration.Release
		}
		if plugin.Configuration.Source != "" {
			module.CompilerSource = plugin.Configuration.Source
		}
		if plugin.Configuration.Target != "" {
			module.CompilerTarget = plugin.Configuration.Target
		}
	}
	module.SourceRoots = []string{resolve(pom.Build.SourceDirectory, "src/main/java")}
	module.TestSourceRoots = []string{resolve(pom.Build.TestSourceDirectory, "src/test/java")}
	module.OutputDirectory = resolve(pom.Build.OutputDirectory, "target/classes")
	module.TestOutputDirectory = resolve(pom.Build.TestOutputDirectory, "target/test-classes")
	for _, resource := range pom.Build.Resources {
		module.ResourceRoots = append(module.ResourceRoots, resolve(resource.Directory, "src/main/resources"))
	}
	if len(module.ResourceRoots) == 0 {
		module.ResourceRoots = []string{filepath.Join(dir, "src/main/resources")}
	}
	for _, resource := range pom.Build.TestResources {
		module.TestResourceRoots = append(module.TestResourceRoots, resolve(resource.Directory, "src/test/resources"))
	}
	if len(module.TestResourceRoots) == 0 {
		module.TestResourceRoots = []string{filepath.Join(dir, "src/test/resources")}
	}
	return module
}

func ResolveClasspath(ctx context.Context, root, moduleDir string, test bool) (Classpath, error) {
	scope := "compile"
	if test {
		scope = "test"
	}
	return resolveClasspath(ctx, root, moduleDir, scope)
}

func ResolveRuntimeClasspath(ctx context.Context, root, moduleDir string) (Classpath, error) {
	return resolveClasspath(ctx, root, moduleDir, "runtime")
}

func resolveClasspath(ctx context.Context, root, moduleDir, scope string) (Classpath, error) {
	root, moduleDir, err := normalizeProjectPaths(root, moduleDir)
	if err != nil {
		return Classpath{}, err
	}
	return resolveClasspathWithPaths(ctx, root, moduleDir, scope)
}

func resolveClasspathWithPaths(ctx context.Context, root, moduleDir, scope string) (Classpath, error) {
	output, err := os.CreateTemp("", "jdtls-go-classpath-*.txt")
	if err != nil {
		return Classpath{}, err
	}
	outputPath := output.Name()
	_ = output.Close()
	defer os.Remove(outputPath)

	args := []string{"-f", filepath.Join(moduleDir, "pom.xml")}
	args = append(args, mavenInvocationArgs(root)...)
	args = append(args,
		"-Dmdep.outputFile="+outputPath,
		"-DincludeScope="+scope,
		"org.apache.maven.plugins:maven-dependency-plugin:"+dependencyPluginVersion+":build-classpath",
	)
	_, err = runMaven(ctx, root, moduleDir, args...)
	if err != nil {
		return Classpath{}, fmt.Errorf("Maven classpath resolution failed: %w", err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		return Classpath{}, fmt.Errorf("Maven did not write the dependency classpath: %w", err)
	}
	model, modelErr := ResolveEffectiveModel(ctx, root, moduleDir)
	var module Module
	var warnings []string
	if modelErr == nil {
		module = model.Module
	} else {
		warnings = append(warnings, "effective Maven model unavailable: "+modelErr.Error())
		pomData, readErr := os.ReadFile(filepath.Join(moduleDir, "pom.xml"))
		if readErr != nil {
			return Classpath{}, readErr
		}
		var pom POM
		if parseErr := xml.Unmarshal(pomData, &pom); parseErr != nil {
			return Classpath{}, parseErr
		}
		module = moduleFromPOM(moduleDir, filepath.Join(moduleDir, "pom.xml"), pom)
	}
	entries := []string{module.OutputDirectory}
	if scope == "test" {
		entries = append([]string{module.TestOutputDirectory}, entries...)
	}
	for _, p := range strings.Split(strings.TrimSpace(string(data)), string(os.PathListSeparator)) {
		if p != "" {
			entries = append(entries, filepath.Clean(p))
		}
	}
	return Classpath{Module: moduleDir, Entries: entries, Separator: string(os.PathListSeparator), Warnings: warnings}, nil
}

func mavenCommand(root string) (string, []string, error) {
	wrapper := filepath.Join(root, "mvnw")
	if info, err := os.Stat(wrapper); err == nil && info.Mode().IsRegular() {
		return wrapper, nil, nil
	}
	if home := os.Getenv("MAVEN_HOME"); home != "" {
		binary := filepath.Join(home, "bin", "mvn")
		if runtime.GOOS == "windows" {
			binary += ".cmd"
		}
		return binary, nil, nil
	}
	if binary, err := exec.LookPath("mvn"); err == nil {
		return binary, nil, nil
	}
	return "", nil, errors.New("Maven 3.9 was not found: install Maven or add mvnw to the project")
}

func buildFallbackClasspath(moduleDir, scope string) (Classpath, error) {
	pomPath := filepath.Join(moduleDir, "pom.xml")
	pomData, err := os.ReadFile(pomPath)
	if err != nil {
		return Classpath{}, err
	}
	var pom POM
	_ = xml.Unmarshal(pomData, &pom)
	module := moduleFromPOM(moduleDir, pomPath, pom)

	entries := []string{module.OutputDirectory}
	if scope == "test" {
		entries = append([]string{module.TestOutputDirectory}, entries...)
	}

	homeDir, _ := os.UserHomeDir()
	localRepo := filepath.Join(homeDir, ".m2", "repository")

	for _, dep := range pom.Dependencies {
		if dep.Scope == "test" && scope != "test" {
			continue
		}
		if dep.GroupID != "" && dep.ArtifactID != "" && dep.Version != "" {
			groupPath := filepath.Join(strings.Split(dep.GroupID, ".")...)
			jarName := fmt.Sprintf("%s-%s.jar", dep.ArtifactID, dep.Version)
			jarPath := filepath.Join(localRepo, groupPath, dep.ArtifactID, dep.Version, jarName)
			if _, statErr := os.Stat(jarPath); statErr == nil {
				entries = append(entries, filepath.Clean(jarPath))
			}
		}
	}

	return Classpath{
		Module:    moduleDir,
		Entries:   entries,
		Separator: string(os.PathListSeparator),
		Warnings:  []string{"using pure-go fallback classpath"},
	}, nil
}
