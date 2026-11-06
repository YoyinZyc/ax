// Copyright 2026 Google LLC
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

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/ax/internal/tunnel"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const pushUsage = `Usage:
  ax push <task-name> --to <remote-context> [flags]

Promote a Task (and its bound Workspace) from the current cluster context to a
target cluster context, suspending the source task and resuming execution on
the target cluster with the same AX_RUN_ID for session/memory continuity.

Flags:
  --to string                 Target Kubernetes context name (required)
  --branch string             Optional Git branch override for the promoted Workspace
  --keep-source-running       Do not suspend the source task before promoting`

type pushFlags struct {
	taskName          string
	toContext         string
	branch            string
	keepSourceRunning bool
	help              bool
}

func parsePushArgs(args []string) (pushFlags, error) {
	if len(args) == 0 {
		return pushFlags{help: true}, nil
	}
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			return pushFlags{help: true}, nil
		}
	}

	var taskName string
	var flagArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if (a == "--to" || a == "--branch") && i+1 < len(args) {
				flagArgs = append(flagArgs, args[i+1])
				i++
			}
			continue
		}
		if taskName == "" {
			taskName = a
		} else {
			return pushFlags{}, fmt.Errorf("unexpected argument %q", a)
		}
	}

	fs := flag.NewFlagSet("ax push", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var out pushFlags
	out.taskName = taskName
	fs.StringVar(&out.toContext, "to", "", "target context")
	fs.StringVar(&out.branch, "branch", "", "git branch override")
	fs.BoolVar(&out.keepSourceRunning, "keep-source-running", false, "keep source running")
	if err := fs.Parse(flagArgs); err != nil {
		return pushFlags{}, err
	}
	if out.taskName == "" {
		return pushFlags{}, fmt.Errorf("task name is required")
	}
	if strings.TrimSpace(out.toContext) == "" {
		return pushFlags{}, fmt.Errorf("--to <remote-context> is required")
	}
	return out, nil
}

func runPush(sourceServerURL, atespace, sourceContext, axNamespace string, args []string) error {
	parsed, err := parsePushArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, pushUsage)
		return err
	}
	if parsed.help {
		fmt.Println(pushUsage)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	srcLabel, _ := tunnel.CurrentContext(sourceContext)
	if srcLabel == "" {
		srcLabel = "current"
	}

	srcClient, srcConn, err := getAXClient(sourceServerURL)
	if err != nil {
		return fmt.Errorf("connecting to source cluster (%s): %w", srcLabel, err)
	}
	defer srcConn.Close()

	fmt.Printf("==> [1/4] Fetching task %q from source context %q...\n", parsed.taskName, srcLabel)
	srcTask, err := srcClient.GetTask(ctx, &v1alpha1.GetTaskRequest{
		Atespace: atespace,
		Name:     parsed.taskName,
	})
	if err != nil {
		return fmt.Errorf("getting task %q on %s: %w", parsed.taskName, srcLabel, err)
	}

	var srcWS *v1alpha1.Workspace
	if wsRef := srcTask.GetSpec().GetWorkspace(); wsRef != nil && wsRef.GetName() != "" {
		ws, err := srcClient.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{
			Atespace: atespace,
			Name:     wsRef.GetName(),
		})
		if err != nil {
			return fmt.Errorf("getting bound workspace %q on %s: %w", wsRef.GetName(), srcLabel, err)
		}
		srcWS = ws
	}

	runID := getEnvValue(srcTask.GetSpec().GetEnv(), "AX_RUN_ID")
	if runID == "" {
		runID = srcTask.GetStatus().GetId()
	}
	if runID == "" {
		runID = fmt.Sprintf("run-%s-%d", parsed.taskName, time.Now().Unix())
	}

	if !parsed.keepSourceRunning {
		fmt.Printf("==> [2/4] Suspending task %q on source context %q (runID=%s)...\n", parsed.taskName, srcLabel, runID)
		if _, err := srcClient.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{
			Atespace: atespace,
			Name:     parsed.taskName,
		}); err != nil && status.Code(err) != codes.FailedPrecondition {
			return fmt.Errorf("suspending source task %q: %w", parsed.taskName, err)
		}
	} else {
		fmt.Printf("==> [2/4] Keeping task %q running on source context %q (runID=%s)...\n", parsed.taskName, srcLabel, runID)
	}

	fmt.Printf("==> [3/4] Connecting to target context %q and promoting manifest...\n", parsed.toContext)
	targetServerURL, err := tunnel.EnsureServerURL(tunnel.Options{
		Context:   parsed.toContext,
		Namespace: axNamespace,
	})
	if err != nil {
		return fmt.Errorf("resolving target cluster %q: %w", parsed.toContext, err)
	}

	dstClient, dstConn, err := getAXClient(targetServerURL)
	if err != nil {
		return fmt.Errorf("connecting to target cluster %q: %w", parsed.toContext, err)
	}
	defer dstConn.Close()

	if srcWS != nil {
		promotedWS := proto.Clone(srcWS).(*v1alpha1.Workspace)
		if parsed.branch != "" {
			for _, g := range promotedWS.GetSpec().GetGit() {
				g.Branch = parsed.branch
			}
		}
		_, _ = dstClient.DeleteWorkspace(ctx, &v1alpha1.DeleteWorkspaceRequest{
			Atespace: atespace,
			Name:     promotedWS.GetMetadata().GetName(),
		})
		if _, err := dstClient.UpdateWorkspace(ctx, &v1alpha1.UpdateWorkspaceRequest{
			Workspace: promotedWS,
		}); err != nil {
			return fmt.Errorf("promoting workspace %q to %s: %w", promotedWS.GetMetadata().GetName(), parsed.toContext, err)
		}
		fmt.Printf("    workspace.ax.io/%s synced to %s\n", promotedWS.GetMetadata().GetName(), parsed.toContext)
	}

	promotedTask := proto.Clone(srcTask).(*v1alpha1.Task)
	promotedTask.Status = nil
	if strings.HasPrefix(promotedTask.GetSpec().GetImage(), "localhost:") || strings.HasPrefix(promotedTask.GetSpec().GetImage(), "127.0.0.1:") {
		promotedTask.Spec.Image = ""
	}
	promotedTask.Spec.Env = upsertEnv(promotedTask.Spec.Env, "AX_RUN_ID", runID)
	promotedTask.Spec.Env = upsertEnv(promotedTask.Spec.Env, "AX_PROMOTED_FROM", srcLabel)
	promotedTask.Spec.Env = upsertEnv(promotedTask.Spec.Env, "AX_CLUSTER_CONTEXT", parsed.toContext)

	_, _ = dstClient.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{
		Atespace: atespace,
		Name:     promotedTask.GetMetadata().GetName(),
	})
	if _, err := dstClient.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: promotedTask,
	}); err != nil {
		return fmt.Errorf("promoting task %q to %s: %w", promotedTask.GetMetadata().GetName(), parsed.toContext, err)
	}
	fmt.Printf("    task.ax.io/%s created on %s (AX_RUN_ID=%s)\n", promotedTask.GetMetadata().GetName(), parsed.toContext, runID)

	fmt.Printf("==> [4/4] Resuming task %q on target context %q...\n", parsed.taskName, parsed.toContext)
	if _, err := dstClient.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{
		Atespace: atespace,
		Name:     parsed.taskName,
	}); err != nil {
		return fmt.Errorf("resuming task %q on %s: %w", parsed.taskName, parsed.toContext, err)
	}
	fmt.Printf("==> Successfully pushed task.ax.io/%s from %q to %q!\n", parsed.taskName, srcLabel, parsed.toContext)
	return nil
}

func getEnvValue(envs []*v1alpha1.EnvVar, key string) string {
	for _, e := range envs {
		if e.GetName() == key {
			return e.GetValue()
		}
	}
	return ""
}

func upsertEnv(envs []*v1alpha1.EnvVar, key, val string) []*v1alpha1.EnvVar {
	for _, e := range envs {
		if e.GetName() == key {
			e.Value = val
			return envs
		}
	}
	return append(envs, &v1alpha1.EnvVar{Name: key, Value: val})
}
