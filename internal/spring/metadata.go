package spring

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Property struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Default     any    `json:"defaultValue"`
	SourceType  string `json:"sourceType"`
}

type metadata struct {
	Properties []Property `json:"properties"`
}

func LoadFromClasspath(entries []string) ([]Property, error) {
	var all []Property
	seen := map[string]bool{}
	for _, entry := range entries {
		if filepath.Ext(entry) != ".jar" {
			continue
		}
		archive, err := zip.OpenReader(entry)
		if err != nil {
			continue
		}
		for _, file := range archive.File {
			if file.Name != "META-INF/spring-configuration-metadata.json" &&
				file.Name != "META-INF/additional-spring-configuration-metadata.json" {
				continue
			}
			rc, err := file.Open()
			if err != nil {
				continue
			}
			var doc metadata
			err = json.NewDecoder(rc).Decode(&doc)
			_ = rc.Close()
			if err != nil {
				continue
			}
			for _, property := range doc.Properties {
				if property.Name != "" && !seen[property.Name] {
					seen[property.Name] = true
					all = append(all, property)
				}
			}
		}
		_ = archive.Close()
	}
	return all, nil
}

func ReadProjectMetadata(path string) ([]Property, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc metadata
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse Spring configuration metadata: %w", err)
	}
	return doc.Properties, nil
}

func Matching(properties []Property, prefix string) []Property {
	prefix = strings.TrimSpace(prefix)
	var matches []Property
	for _, property := range properties {
		if strings.HasPrefix(property.Name, prefix) {
			matches = append(matches, property)
		}
	}
	return matches
}
