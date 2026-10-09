// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hubtree

import (
	"fmt"
	"strings"
	"testing"
)

type coverageFixture struct {
	name  string
	nodes []Node
	dirs  []string
}

func coverageFixtures() []coverageFixture {
	var fixtures []coverageFixture
	for _, width := range []int{100, 1000, 5000} {
		f := coverageFixture{name: fmt.Sprintf("wide-%d", width)}
		for i := 0; i < width; i++ {
			dir := fmt.Sprintf("d%d", i)
			f.dirs = append(f.dirs, dir)
			f.nodes = append(f.nodes, dirNode(dir))
		}
		for _, dir := range f.dirs {
			f.nodes = append(f.nodes, fileNode(dir+"/file", 1))
		}
		fixtures = append(fixtures, f)
	}
	for _, depth := range []int{32, 256} {
		f := coverageFixture{name: fmt.Sprintf("deep-%d", depth)}
		dir := "root"
		for i := 0; i < depth; i++ {
			f.dirs = append(f.dirs, dir)
			f.nodes = append(f.nodes, dirNode(dir))
			dir += "/segment"
		}
		f.nodes = append(f.nodes, fileNode(dir+"/file", 1))
		fixtures = append(fixtures, f)
	}
	f := coverageFixture{name: "shared-depth-64-width-500"}
	parent := strings.Repeat("segment/", 64) + "root"
	for i := 0; i < 500; i++ {
		dir := fmt.Sprintf("%s/d%d", parent, i)
		f.dirs = append(f.dirs, dir)
		f.nodes = append(f.nodes, dirNode(dir))
	}
	for _, dir := range f.dirs {
		f.nodes = append(f.nodes, fileNode(dir+"/file", 1))
	}
	return append(fixtures, f)
}

var benchmarkCovered int

// Each operation represents every directory decision for one complete listing.
// Fixture construction is excluded; production index construction is timed.
func BenchmarkSubtreeCoverage(b *testing.B) {
	for _, f := range coverageFixtures() {
		b.Run(f.name+"/scan", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				count := 0
				for _, dir := range f.dirs {
					if scanSubtreeListed(f.nodes, dir) {
						count++
					}
				}
				benchmarkCovered = count
			}
		})
		b.Run(f.name+"/index", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				covered := coveredSubtrees(f.nodes)
				count := 0
				for _, dir := range f.dirs {
					if covered[dir] {
						count++
					}
				}
				benchmarkCovered = count
			}
		})
	}
}
