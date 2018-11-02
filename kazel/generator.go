/*
Copyright 2017 The Kubernetes Authors.

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

package main

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bazelbuild/buildtools/build"
)

var (
	// Generator tags are specified using the format "// +k8s:name=value"
	genTagRe = regexp.MustCompile(`//\s*\+k8s:([^\s=]+)(?:=(\S+))\s*\n`)
)

// {tagName: {value: {pkgs}}}
type generatorTagsValuesPkgsMap map[string]map[string]map[string]bool

// extractTags finds k8s codegen tags found in b listed in requestedTags.
// It returns a map of {tag name: slice of values for that tag}.
func extractTags(b []byte, requestedTags map[string]bool) map[string][]string {
	tags := make(map[string][]string)
	matches := genTagRe.FindAllSubmatch(b, -1)
	for _, m := range matches {
		if len(m) >= 3 {
			tag, values := string(m[1]), string(m[2])
			if _, requested := requestedTags[tag]; !requested {
				continue
			}
			tags[tag] = append(tags[tag], strings.Split(values, ",")...)
		}
	}
	return tags
}

// findGeneratorTags searches for all packages under root that include a kubernetes generator
// tag comment. It does not follow symlinks, and any path in the configured skippedPaths
// or codegen skipped paths is skipped.
func (v *Vendorer) findGeneratorTags(root string, requestedTags map[string]bool) (generatorTagsValuesPkgsMap, error) {
	tagsValuesPkgs := make(generatorTagsValuesPkgsMap)

	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		pkg := filepath.Dir(path)

		for _, r := range v.skippedK8sCodegenPaths {
			if r.MatchString(pkg) {
				return filepath.SkipDir
			}
		}

		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		b, err := ioutil.ReadFile(path)
		if err != nil {
			return err
		}

		for tag, values := range extractTags(b, requestedTags) {
			if _, present := tagsValuesPkgs[tag]; !present {
				tagsValuesPkgs[tag] = make(map[string]map[string]bool)
			}
			for _, v := range values {
				if _, present := tagsValuesPkgs[tag][v]; !present {
					tagsValuesPkgs[tag][v] = make(map[string]bool)
				}
				// Since multiple files in the same package may list a given tag/value, use a set to deduplicate.
				tagsValuesPkgs[tag][v][pkg] = true
			}
		}

		return nil
	})

	if walkErr != nil {
		return nil, walkErr
	}

	return tagsValuesPkgs, nil
}

// flattened returns a copy of the map with the final stringSet flattened into a sorted slice.
func flattened(m generatorTagsValuesPkgsMap) map[string]map[string][]string {
	flattened := make(map[string]map[string][]string)
	for tag, subMap := range m {
		flattened[tag] = make(map[string][]string)
		for k, subSet := range subMap {
			for v := range subSet {
				flattened[tag][k] = append(flattened[tag][k], v)
			}
			sort.Strings(flattened[tag][k])
		}
	}
	return flattened
}

// walkGenerated generates a k8s codegen bzl file that can be parsed by Starlark
// rules and macros to find packages needed k8s code generation.
// This involves reading all non-test go sources in the tree and looking for
// "+k8s:name=value" tags. Only those tags listed in K8sCodegenTags will be
// included.
// If a K8sCodegenBoilerplateFile was configured, the contents of this file
// will be included as the header of the generated bzl file.
// Returns true if there are diffs against the existing generated bzl file.
func (v *Vendorer) walkGenerated() (bool, error) {
	if v.cfg.K8sCodegenBzlFile == "" {
		return false, nil
	}
	// only include the specified tags
	requestedTags := make(map[string]bool)
	for _, tag := range v.cfg.K8sCodegenTags {
		requestedTags[tag] = true
	}
	tagsValuesPkgs, err := v.findGeneratorTags(".", requestedTags)
	if err != nil {
		return false, err
	}

	f := &build.File{
		Path: v.cfg.K8sCodegenBzlFile,
	}
	addCommentBefore(f, "#################################################")
	addCommentBefore(f, "# # # # # # # # # # # # # # # # # # # # # # # # #")
	addCommentBefore(f, "This file is autogenerated by kazel. DO NOT EDIT.")
	addCommentBefore(f, "# # # # # # # # # # # # # # # # # # # # # # # # #")
	addCommentBefore(f, "#################################################")
	addCommentBefore(f, "")

	f.Stmt = append(f.Stmt, varExpr("go_prefix", "The go prefix passed to kazel", v.cfg.GoPrefix))
	f.Stmt = append(f.Stmt, varExpr("kazel_configured_tags", "The list of codegen tags kazel is configured to find", v.cfg.K8sCodegenTags))
	f.Stmt = append(f.Stmt, varExpr("tags_values_pkgs", "tags_values_pkgs is a dictionary mapping {k8s build tag: {tag value: [pkgs including that tag:value]}}", flattened(tagsValuesPkgs)))

	var boilerplate []byte
	if v.cfg.K8sCodegenBoilerplateFile != "" {
		boilerplate, err = ioutil.ReadFile(v.cfg.K8sCodegenBoilerplateFile)
		if err != nil {
			return false, err
		}
	}
	// Open existing file to use in diff mode.
	_, err = os.Stat(f.Path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return writeFile(f.Path, f, boilerplate, !os.IsNotExist(err), v.dryRun)
}
