//  Copyright (c) 2018 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package builder

import (
	"fmt"
	"strconv"

	"github.com/uber/makisu/lib/builder/step"
	"github.com/uber/makisu/lib/cache"
	"github.com/uber/makisu/lib/context"
	"github.com/uber/makisu/lib/docker/image"
	"github.com/uber/makisu/lib/log"
	"github.com/uber/makisu/lib/parser/dockerfile"
	"github.com/uber/makisu/lib/utils/stringset"
)

// BuildPlan describes a list of named buildStages, that can copy files between
// one another.
type BuildPlan struct {
	baseCtx           *context.BuildContext
	copyFromDirs      map[string][]string
	target            image.Name
	cacheMgr          cache.Manager
	stages            []*buildStage
	remoteImageStages map[string]*buildStage
	allowModifyFS     bool
	forceCommit       bool
}

// NewBuildPlan takes in contextDir, a target image and an ImageStore, and
// returns a new BuildPlan.
func NewBuildPlan(
	ctx *context.BuildContext, target image.Name,
	cacheMgr cache.Manager, parsedStages []*dockerfile.Stage,
	allowModifyFS, forceCommit bool) (*BuildPlan, error) {

	plan := &BuildPlan{
		baseCtx:           ctx,
		copyFromDirs:      make(map[string][]string),
		target:            target,
		cacheMgr:          cacheMgr,
		stages:            make([]*buildStage, len(parsedStages)),
		remoteImageStages: make(map[string]*buildStage),
		allowModifyFS:     allowModifyFS,
		forceCommit:       forceCommit,
	}

	aliases, err := buildAliases(parsedStages)
	if err != nil {
		return nil, fmt.Errorf("build alias list: %v", err)
	}

	digestPairs := make(image.DigestPairMap)
	for i, parsedStage := range parsedStages {
		steps, err := step.NewDockerfileSteps(plan.baseCtx, parsedStage)
		if err != nil {
			return nil, fmt.Errorf("new dockerfile steps: %v", err)
		}

		// Add this stage to the plan.
		stage, err := newBuildStage(plan.baseCtx, parsedStage.From.Alias,
			steps, digestPairs, plan.allowModifyFS, plan.forceCommit)
		if err != nil {
			return nil, fmt.Errorf("failed to convert parsed stage: %s", err)
		}

		if len(stage.copyFromDirs) > 0 && !plan.allowModifyFS {
			// TODO(pourchet): Support this at some point.
			return nil, fmt.Errorf("must allow modifyfs for multi-stage dockerfiles with COPY --from")
		}
		plan.stages[i] = stage
	}

	if err := plan.handleCopyFromDirs(aliases, digestPairs); err != nil {
		return nil, fmt.Errorf("handle cross refs: %v", err)
	}
	return plan, nil
}

// handleCopyFromDirs goes through all of the stages in the build plan and looks at the `COPY --from` steps
// to make sure they are valid. If the --from source is another image, we create a new image stage in
// the build plan.
func (plan *BuildPlan) handleCopyFromDirs(aliases map[string]bool, digestPairs image.DigestPairMap) error {
	for _, stage := range plan.stages {
		for alias, dirs := range stage.copyFromDirs {
			if _, ok := aliases[alias]; !ok {
				// If we see that the alias of the cross referenced directory is an image name,
				// we add a fake stage to the build plan that will download that image directly
				// into the cross referencing root for that alias.
				name, err := image.ParseNameForPull(alias)
				if err != nil || !name.IsValid() {
					return fmt.Errorf("copy from nonexistent stage %s", alias)
				}
				remoteImageStage, err := plan.newRemoteImageStage(alias, digestPairs)
				if err != nil {
					return fmt.Errorf("new image stage: %v", err)
				}
				plan.remoteImageStages[alias] = remoteImageStage
				aliases[alias] = true
			}
			plan.copyFromDirs[alias] = stringset.FromSlice(
				append(plan.copyFromDirs[alias], dirs...),
			).ToSlice()
		}
	}
	return nil
}

// buildAliases mutates the list of stages to assign default aliases. Those will be integers starting
// from 0.
func buildAliases(stages dockerfile.Stages) (map[string]bool, error) {
	aliases := make(map[string]bool)
	for i, parsedStage := range stages {
		// Check for stage alias collision if alias isn't empty.
		if parsedStage.From.Alias != "" {
			if _, ok := aliases[parsedStage.From.Alias]; ok {
				return nil, fmt.Errorf("duplicate stage alias: %s", parsedStage.From.Alias)
			} else if _, err := strconv.Atoi(parsedStage.From.Alias); err == nil {
				// Docker would return `name can't start with a number or contain symbols`
				return nil, fmt.Errorf("stage alias cannot be a number: %s", parsedStage.From.Alias)
			}
		} else {
			parsedStage.From.Alias = strconv.Itoa(i)
		}
		aliases[parsedStage.From.Alias] = true
	}
	return aliases, nil
}

func (plan *BuildPlan) newRemoteImageStage(alias string, digestPairs image.DigestPairMap) (*buildStage, error) {
	from, err := step.NewFromStep(alias, alias, alias)
	if err != nil {
		return nil, fmt.Errorf("new from step: %v", err)
	}
	steps := []step.BuildStep{from}
	stage, err := newBuildStage(plan.baseCtx, alias, steps, digestPairs, plan.allowModifyFS, false)
	if err != nil {
		return nil, fmt.Errorf("new build stage: %v", err)
	}
	return stage, nil
}

// Execute executes all build stages in order.
func (plan *BuildPlan) Execute() (*image.DistributionManifest, error) {
	// Execute pre-build procedures. Try to pull some reusable layers from the registry.
	// TODO: in parallel
	for _, stage := range plan.stages {
		stage.pullCacheLayers(plan.cacheMgr)
	}

	for alias, stage := range plan.remoteImageStages {
		// Building that pseudo stage will unpack the image directly into the stage's
		// cross stage directory.
		name, err := image.ParseNameForPull(alias)
		if err != nil {
			return nil, fmt.Errorf("failed to parse cross stage reference name %v: %v", alias, err)
		}
		log.Infof("Pulling image %v for cross stage reference", name)
		if err := stage.build(plan.cacheMgr, false, true); err != nil {
			return nil, fmt.Errorf("build stage %v for cross stage reference: %v", name, err)
		} else if err := stage.checkpoint(plan.copyFromDirs[alias]); err != nil {
			return nil, fmt.Errorf("stage checkpoint %v for cross stage reference: %v", name, err)
		} else if err := stage.cleanup(); err != nil {
			return nil, fmt.Errorf("stage cleanup %v for cross stage reference: %v", name, err)
		}
	}

	var currStage *buildStage
	for k := 0; k < len(plan.stages); k++ {
		currStage = plan.stages[k]
		log.Infof("* Stage %d/%d : %s", k+1, len(plan.stages), currStage.String())

		lastStage := k == len(plan.stages)-1
		_, copiedFrom := plan.copyFromDirs[currStage.alias]
		if err := currStage.build(plan.cacheMgr, lastStage, copiedFrom); err != nil {
			return nil, fmt.Errorf("build stage: %s", err)
		}

		if plan.allowModifyFS {
			if k < len(plan.stages)-1 {
				// Save context directories needed for cross-stage copy operations.
				copyFromDirs := plan.copyFromDirs[currStage.alias]
				if err := currStage.checkpoint(copyFromDirs); err != nil {
					return nil, fmt.Errorf("checkpoint memfs: %s", err)
				}
			}

			if err := currStage.cleanup(); err != nil {
				return nil, fmt.Errorf("remove memfs: %s", err)
			}
		}
	}

	// Wait for cache layers to be pushed.
	// This will make them available to other builds ongoing on different machines.
	if err := plan.cacheMgr.WaitForPush(); err != nil {
		log.Errorf("Failed to push cache: %s", err)
	}

	// Save image.
	repo, tag := plan.target.GetRepository(), plan.target.GetTag()
	manifest, err := currStage.saveImage(plan.baseCtx.ImageStore, repo, tag)
	if err != nil {
		return nil, fmt.Errorf("save context image: %s", err)
	}

	// Print out the image size.
	size := int64(0)
	for _, layer := range manifest.Layers {
		size += layer.Size
	}
	log.Infow(fmt.Sprintf("Computed total image size %d", size), "total_image_size", size)

	return manifest, nil
}
