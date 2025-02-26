package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/go-gorp/gorp"
	"github.com/rockbears/log"

	"github.com/ovh/cds/engine/api/repositoriesmanager"
	"github.com/ovh/cds/engine/cache"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/telemetry"
)

type VCSEventMessenger struct {
	commitsStatuses map[string][]sdk.VCSCommitStatus
	vcsClient       sdk.VCSAuthorizedClientService
}

// ResyncCommitStatus resync commit status for a workflow run
func ResyncCommitStatus(ctx context.Context, db *gorp.DbMap, store cache.Store, proj sdk.Project, wr *sdk.WorkflowRun) error {
	_, end := telemetry.Span(ctx, "workflow.resyncCommitStatus",
		telemetry.Tag(telemetry.TagWorkflow, wr.Workflow.Name),
		telemetry.Tag(telemetry.TagWorkflowRun, wr.Number),
	)
	defer end()

	eventMessenger := &VCSEventMessenger{commitsStatuses: make(map[string][]sdk.VCSCommitStatus)}
	for _, nodeRuns := range wr.WorkflowNodeRuns {
		sort.Slice(nodeRuns, func(i, j int) bool {
			return nodeRuns[i].SubNumber >= nodeRuns[j].SubNumber
		})
		nodeRun := nodeRuns[0]

		if err := eventMessenger.SendVCSEvent(ctx, db, store, proj, *wr, nodeRun); err != nil {
			log.Error(ctx, "resyncCommitStatus > unable to send vcs event: %v", err)
		}
	}

	return nil
}

func (e *VCSEventMessenger) SendVCSEvent(ctx context.Context, db *gorp.DbMap, store cache.Store, proj sdk.Project, wr sdk.WorkflowRun, nodeRun sdk.WorkflowNodeRun) error {
	tx, err := db.Begin()
	if err != nil {
		return sdk.WithStack(err)
	}
	defer tx.Rollback() // nolint

	if nodeRun.Status == sdk.StatusWaiting {
		return nil
	}

	if e.commitsStatuses == nil {
		e.commitsStatuses = make(map[string][]sdk.VCSCommitStatus)
	}

	node := wr.Workflow.WorkflowData.NodeByID(nodeRun.WorkflowNodeID)
	if !node.IsLinkedToRepo(&wr.Workflow) {
		return nil
	}

	var notif *sdk.WorkflowNotification
	// browse notification to find vcs one
loopNotif:
	for _, n := range wr.Workflow.Notifications {
		if n.Type != sdk.VCSUserNotification {
			continue
		}
		// If list of node is nill, send notification to all of them
		if len(n.NodeIDs) == 0 {
			notif = &n
			break
		}
		// browser source node id
		for _, src := range n.NodeIDs {
			if src == node.ID {
				notif = &n
				break loopNotif
			}
		}
	}

	if notif == nil {
		return nil
	}

	vcsServerName := wr.Workflow.Applications[node.Context.ApplicationID].VCSServer
	repoFullName := wr.Workflow.Applications[node.Context.ApplicationID].RepositoryFullname

	//Get the RepositoriesManager Client
	if e.vcsClient == nil {
		var err error
		e.vcsClient, err = repositoriesmanager.AuthorizedClient(ctx, tx, store, proj.Key, vcsServerName)
		if err != nil {
			return sdk.WrapError(err, "can't get AuthorizedClient for %v/%v", proj.Key, vcsServerName)
		}
	}

	ref := nodeRun.VCSHash
	if nodeRun.VCSTag != "" {
		ref = nodeRun.VCSTag
	}

	statuses, ok := e.commitsStatuses[ref]
	if !ok {
		var err error
		statuses, err = e.vcsClient.ListStatuses(ctx, repoFullName, ref)
		if err != nil {
			return sdk.WrapError(err, "can't ListStatuses for %v with vcs %v/%v", repoFullName, proj.Key, vcsServerName)
		}
		e.commitsStatuses[ref] = statuses
	}
	expected := sdk.VCSCommitStatusDescription(proj.Key, wr.Workflow.Name, sdk.EventRunWorkflowNode{
		NodeName: nodeRun.WorkflowNodeName,
	})

	if e.vcsClient.IsBitbucketCloud() {
		if len(expected) > 36 { // 40 maxlength on bitbucket cloud
			expected = expected[:36]
		}
	}

	var statusFound *sdk.VCSCommitStatus
	for i, status := range statuses {
		if status.Decription == expected {
			statusFound = &statuses[i]
			break
		}
	}

	if statusFound == nil || statusFound.State == "" {
		if err := e.sendVCSEventStatus(ctx, tx, store, proj.Key, wr, &nodeRun, notif, vcsServerName); err != nil {
			return sdk.WrapError(err, "can't sendVCSEventStatus vcs %v/%v", proj.Key, vcsServerName)
		}
	} else {
		skipStatus := false
		switch statusFound.State {
		case sdk.StatusSuccess:
			switch nodeRun.Status {
			case sdk.StatusSuccess:
				skipStatus = true
			}
		case sdk.StatusFail:
			switch nodeRun.Status {
			case sdk.StatusFail:
				skipStatus = true
			}

		case sdk.StatusSkipped:
			switch nodeRun.Status {
			case sdk.StatusDisabled, sdk.StatusNeverBuilt, sdk.StatusSkipped:
				skipStatus = true
			}
		}

		if !skipStatus {
			if err := e.sendVCSEventStatus(ctx, tx, store, proj.Key, wr, &nodeRun, notif, vcsServerName); err != nil {
				return sdk.WrapError(err, "can't sendVCSEventStatus vcs %v/%v", proj.Key, vcsServerName)
			}
		}
	}

	if !sdk.StatusIsTerminated(nodeRun.Status) {
		return nil
	}
	if err := e.sendVCSPullRequestComment(ctx, tx, wr, &nodeRun, notif, vcsServerName); err != nil {
		return sdk.WrapError(err, "can't sendVCSPullRequestComment vcs %v/%v", proj.Key, vcsServerName)
	}

	if err := tx.Commit(); err != nil {
		return sdk.WithStack(err)
	}

	return nil
}

// sendVCSEventStatus send status
func (e *VCSEventMessenger) sendVCSEventStatus(ctx context.Context, db gorp.SqlExecutor, store cache.Store, projectKey string, wr sdk.WorkflowRun, nodeRun *sdk.WorkflowNodeRun, notif *sdk.WorkflowNotification, vcsServerName string) error {
	if notif == nil || notif.Settings.Template == nil || (notif.Settings.Template.DisableStatus != nil && *notif.Settings.Template.DisableStatus) {
		return nil
	}

	log.Debug(ctx, "Send status for node run %d", nodeRun.ID)
	var app sdk.Application
	var pip sdk.Pipeline
	var env sdk.Environment
	node := wr.Workflow.WorkflowData.NodeByID(nodeRun.WorkflowNodeID)
	if !node.IsLinkedToRepo(&wr.Workflow) {
		return nil
	}

	app = wr.Workflow.Applications[node.Context.ApplicationID]
	if node.Context.PipelineID > 0 {
		pip = wr.Workflow.Pipelines[node.Context.PipelineID]
	}
	if node.Context.EnvironmentID > 0 {
		env = wr.Workflow.Environments[node.Context.EnvironmentID]
	}

	var eventWNR = sdk.EventRunWorkflowNode{
		ID:             nodeRun.ID,
		Number:         nodeRun.Number,
		SubNumber:      nodeRun.SubNumber,
		Status:         nodeRun.Status,
		Start:          nodeRun.Start.Unix(),
		Done:           nodeRun.Done.Unix(),
		Manual:         nodeRun.Manual,
		HookEvent:      nodeRun.HookEvent,
		Payload:        nodeRun.Payload,
		SourceNodeRuns: nodeRun.SourceNodeRuns,
		Hash:           nodeRun.VCSHash,
		Tag:            nodeRun.VCSTag,
		BranchName:     nodeRun.VCSBranch,
		NodeID:         nodeRun.WorkflowNodeID,
		RunID:          nodeRun.WorkflowRunID,
		StagesSummary:  make([]sdk.StageSummary, len(nodeRun.Stages)),
		NodeName:       nodeRun.WorkflowNodeName,
	}

	for i := range nodeRun.Stages {
		eventWNR.StagesSummary[i] = nodeRun.Stages[i].ToSummary()
	}

	var pipName, appName, envName string

	pipName = pip.Name
	appName = app.Name
	eventWNR.RepositoryManagerName = app.VCSServer
	eventWNR.RepositoryFullName = app.RepositoryFullname

	if env.Name != "" {
		envName = env.Name
	}

	report, err := nodeRun.Report()
	if err != nil {
		return err
	}

	// Check if it's a gerrit or not
	isGerrit, err := e.vcsClient.IsGerrit(ctx, db)
	if err != nil {
		return err
	}
	if isGerrit {
		// Get gerrit variable
		var project, changeID, branch, revision, url string
		projectParam := sdk.ParameterFind(nodeRun.BuildParameters, "git.repository")
		if projectParam != nil {
			project = projectParam.Value
		}
		changeIDParam := sdk.ParameterFind(nodeRun.BuildParameters, "gerrit.change.id")
		if changeIDParam != nil {
			changeID = changeIDParam.Value
		}
		branchParam := sdk.ParameterFind(nodeRun.BuildParameters, "gerrit.change.branch")
		if branchParam != nil {
			branch = branchParam.Value
		}
		revisionParams := sdk.ParameterFind(nodeRun.BuildParameters, "git.hash")
		if revisionParams != nil {
			revision = revisionParams.Value
		}
		urlParams := sdk.ParameterFind(nodeRun.BuildParameters, "cds.ui.pipeline.run")
		if urlParams != nil {
			url = urlParams.Value
		}
		if changeID != "" {
			eventWNR.GerritChange = &sdk.GerritChangeEvent{
				ID:         changeID,
				DestBranch: branch,
				Project:    project,
				Revision:   revision,
				Report:     report,
				URL:        url,
			}
		}
	}

	payload, _ := json.Marshal(eventWNR)

	evt := sdk.Event{
		EventType:       fmt.Sprintf("%T", eventWNR),
		Payload:         payload,
		Timestamp:       time.Now(),
		ProjectKey:      projectKey,
		WorkflowName:    wr.Workflow.Name,
		PipelineName:    pipName,
		ApplicationName: appName,
		EnvironmentName: envName,
	}

	if err := e.vcsClient.SetStatus(ctx, evt, e.vcsClient.IsDisableStatusDetails(ctx)); err != nil {
		if err2 := repositoriesmanager.RetryEvent(&evt, err, store); err2 != nil {
			return err2
		}
		return err
	}

	return nil
}

func (e *VCSEventMessenger) sendVCSPullRequestComment(ctx context.Context, db gorp.SqlExecutor, wr sdk.WorkflowRun, nodeRun *sdk.WorkflowNodeRun, notif *sdk.WorkflowNotification, vcsServerName string) error {
	if notif == nil || notif.Settings.Template == nil || (notif.Settings.Template.DisableComment != nil && *notif.Settings.Template.DisableComment) {
		return nil
	}

	if nodeRun.Status != sdk.StatusFail && nodeRun.Status != sdk.StatusStopped && notif.Settings.OnSuccess != sdk.UserNotificationAlways {
		return nil
	}

	log.Debug(ctx, "Send pull-request comment for node run %d", nodeRun.ID)

	var app sdk.Application
	node := wr.Workflow.WorkflowData.NodeByID(nodeRun.WorkflowNodeID)
	if !node.IsLinkedToRepo(&wr.Workflow) {
		return nil
	}

	if nodeRun.VCSReport == "" {
		nodeRun.VCSReport = notif.Settings.Template.Body
	}

	app = wr.Workflow.Applications[node.Context.ApplicationID]

	report, err := nodeRun.Report()
	if err != nil {
		return err
	}

	var changeID string
	changeIDParam := sdk.ParameterFind(nodeRun.BuildParameters, "gerrit.change.id")
	if changeIDParam != nil {
		changeID = changeIDParam.Value
	}

	var revision string
	revisionParams := sdk.ParameterFind(nodeRun.BuildParameters, "git.hash")
	if revisionParams != nil {
		revision = revisionParams.Value
	}

	reqComment := sdk.VCSPullRequestCommentRequest{Message: report}
	reqComment.Revision = revision

	isGerrit, err := e.vcsClient.IsGerrit(ctx, db)
	if err != nil {
		return err
	}

	if changeID != "" && isGerrit {
		reqComment.ChangeID = changeID
		if err := e.vcsClient.PullRequestComment(ctx, app.RepositoryFullname, reqComment); err != nil {
			return err
		}
	} else if !isGerrit {
		//Check if this branch and this commit is a pullrequest
		prs, err := e.vcsClient.PullRequests(ctx, app.RepositoryFullname)
		if err != nil {
			return err
		}

		//Send comment on pull request
		for _, pr := range prs {
			if pr.Head.Branch.DisplayID == nodeRun.VCSBranch && IsSameCommit(pr.Head.Branch.LatestCommit, nodeRun.VCSHash) && !pr.Merged && !pr.Closed {
				reqComment.ID = pr.ID
				if err := e.vcsClient.PullRequestComment(ctx, app.RepositoryFullname, reqComment); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

func IsSameCommit(sha1, sha1b string) bool {
	if len(sha1) == len(sha1b) {
		return sha1 == sha1b
	}
	if len(sha1) == 12 && len(sha1b) >= 12 {
		return sha1 == sha1b[0:len(sha1)]
	}
	if len(sha1b) == 12 && len(sha1) >= 12 {
		return sha1b == sha1[0:len(sha1b)]
	}
	return false
}
