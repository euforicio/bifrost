// Verify the source/continuation credential and backend-ordering boundaries.
package main

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
)

type step struct {
	ID, Name, Run, Uses, If string
	Env, With               map[string]string
}
type job struct {
	If    string
	Needs any
	Steps []step
}
type workflow struct {
	Concurrency struct {
		Group, Queue string
		Cancel       bool `yaml:"cancel-in-progress"`
	}
	Jobs map[string]job
}

var root = "."

func read(path string) workflow {
	raw, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		panic(err)
	}
	var w workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		panic(err)
	}
	return w
}
func check(source, resume workflow) error {
	if source.Concurrency.Group != resume.Concurrency.Group || source.Concurrency.Queue != "max" || resume.Concurrency.Queue != "max" || source.Concurrency.Cancel || resume.Concurrency.Cancel {
		return fmt.Errorf("production continuations must serialize without replacing pending runs")
	}
	if !strings.Contains(resume.Jobs["qualify"].If, "CLOUDFLARE_RELEASE_QUEUE_ENABLED == 'true'") || !strings.Contains(resume.Jobs["qualify"].If, "refs/heads/main") || !strings.Contains(resume.Jobs["deploy"].If, "CLOUDFLARE_RELEASE_QUEUE_ENABLED == 'true'") || resume.Jobs["deploy"].Needs != "qualify" {
		return fmt.Errorf("resume must require qualification and dual activation gates")
	}
	guarded := 0
	for _, s := range source.Jobs["promote-cloudflare"].Steps {
		if s.Name == "Verify checked backend before frontend deployment" || s.Name == "Deploy checked frontend CDN assets" {
			if s.If != "steps.promotion.outputs.release_ready == 'true'" {
				return fmt.Errorf("queued frontend must not publish before backend landing")
			}
			guarded++
		}
	}
	if guarded != 2 {
		return fmt.Errorf("source backend/frontend guards absent")
	}
	exactInfra, exactSource, artifact, verify := false, false, false, false
	for _, s := range resume.Jobs["deploy"].Steps {
		if strings.HasPrefix(s.Uses, "actions/checkout@") {
			if s.With["repository"] == "euforicio/orion-infra" {
				exactInfra = s.With["ref"] == "${{ needs.qualify.outputs.infra_sha }}"
			} else {
				exactSource = s.With["ref"] == "${{ needs.qualify.outputs.source_sha }}"
			}
		}
		if strings.HasPrefix(s.Uses, "actions/download-artifact@") {
			artifact = s.With["name"] == "bifrost-web" && s.With["run-id"] == "${{ inputs.release_run }}"
		}
		if strings.Contains(s.Run, "wrangler deploy") {
			verify = strings.Contains(s.Run, "require-release-run.mjs bifrost") && strings.Contains(s.Run, "verify-product-release.mjs bifrost") && strings.Index(s.Run, "verify-product-release") < strings.Index(s.Run, "wrangler deploy")
		}
	}
	if !exactInfra || !exactSource || !artifact || !verify {
		return fmt.Errorf("resume must use bound source/helper/artifact and reverify before deployment")
	}
	return nil
}
func main() {
	if len(os.Args) == 2 {
		root = os.Args[1]
	}
	const original = ".github/workflows/orion-image.yml"
	const continuation = ".github/workflows/cloudflare-frontend.yml"
	if err := check(read(original), read(continuation)); err != nil {
		panic(err)
	}
	// Guard against tautological contracts: actual parsed-workflow regressions must fail.
	for _, mutation := range []func(*workflow){
		func(w *workflow) { j := w.Jobs["deploy"]; j.If = ""; w.Jobs["deploy"] = j },
		func(w *workflow) { w.Concurrency.Group = "unserialized" },
		func(w *workflow) {
			j := w.Jobs["deploy"]
			for i := range j.Steps {
				if j.Steps[i].With["repository"] == "euforicio/orion-infra" {
					j.Steps[i].With["ref"] = "main"
				}
			}
			w.Jobs["deploy"] = j
		},
		func(w *workflow) {
			j := w.Jobs["deploy"]
			for i := range j.Steps {
				if strings.HasPrefix(j.Steps[i].Uses, "actions/download-artifact@") {
					j.Steps[i].With["run-id"] = "unbound"
				}
			}
			w.Jobs["deploy"] = j
		},
	} {
		broken := read(continuation)
		mutation(&broken)
		if check(read(original), broken) == nil {
			panic("workflow regression unexpectedly passed")
		}
	}
	fmt.Println("Cloudflare queued-source and exact-run frontend workflow contracts passed")
}
