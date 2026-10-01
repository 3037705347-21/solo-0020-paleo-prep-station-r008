package main

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"example.com/paleo-prep-station/internal/contracts"
)

type planHoldSnapshot struct {
	Plan     planHoldView `json:"plan"`
	Progress []struct {
		Percent int `json:"percent"`
	} `json:"progress"`
}

// checkHoldAndResume 覆盖暂停/恢复的成功路径、公开读取一致性、非法状态、
// 重复操作、非法输入以及并发请求的确定结果。
func checkHoldAndResume(client *http.Client, baseURL string) error {
	cases := []struct {
		catalog string
		run     func(*http.Client, string, string) error
	}{
		{"PPS-2026-H01", checkPauseResumeLifecycle},
		{"PPS-2026-H02", checkProgressBlockedWhilePaused},
		{"PPS-2026-H03", checkDoublePause},
		{"PPS-2026-H04", checkDoubleResume},
		{"PPS-2026-H05", checkPauseRejectedByState},
		{"PPS-2026-H06", checkResumeRejectedByState},
		{"PPS-2026-H07", checkInvalidPauseRequest},
		{"PPS-2026-H08", checkConcurrentPauses},
		{"PPS-2026-H09", checkConcurrentResumes},
		{"PPS-2026-H10", checkConcurrentProgressWhilePaused},
		{"PPS-2026-H11", checkConcurrentPauseVsProgress},
		{"PPS-2026-H12", checkConcurrentResumeVsProgress},
	}
	for _, tc := range cases {
		if err := tc.run(client, baseURL, tc.catalog); err != nil {
			return fmt.Errorf("%s: %w", tc.catalog, err)
		}
	}
	return nil
}

// 活动方案带原因和责任人暂停后，标本与方案公开读取保持一致；当前负责人
// 恢复后两个聚合再次一致，随后可继续写进度直至完成。
func checkPauseResumeLifecycle(client *http.Client, baseURL, catalog string) error {
	specimen, plan, err := activePlanSetup(client, baseURL, catalog)
	if err != nil {
		return err
	}
	if err := recordProgress(client, baseURL, plan.ID, 30, "初步清修"); err != nil {
		return err
	}

	paused, err := pausePlan(client, baseURL, plan.ID,
		contracts.PausePlanRequest{Reason: "展柜外借，制备台被占用", PausedBy: "keeper-zhou"},
		http.StatusOK)
	if err != nil {
		return err
	}
	if paused.Status != "paused" {
		return fmt.Errorf("expected paused plan, got %s", paused.Status)
	}
	if len(paused.Pauses) != 1 || paused.Pauses[0].Reason != "展柜外借，制备台被占用" ||
		paused.Pauses[0].PausedBy != "keeper-zhou" || paused.Pauses[0].ResumedAt != nil {
		return fmt.Errorf("pause record missing reason/owner or already closed: %+v", paused.Pauses)
	}
	if err := expectPlanSpecimenConsistency(client, baseURL, plan.ID, specimen.ID, "paused", "on_hold"); err != nil {
		return err
	}

	resumed, err := resumePlan(client, baseURL, plan.ID,
		contracts.ResumePlanRequest{ResumedBy: "preparator-chen"},
		http.StatusOK)
	if err != nil {
		return err
	}
	if resumed.Status != "active" {
		return fmt.Errorf("expected active plan after resume, got %s", resumed.Status)
	}
	if len(resumed.Pauses) != 1 || resumed.Pauses[0].ResumedBy != "preparator-chen" ||
		resumed.Pauses[0].ResumedAt == nil {
		return fmt.Errorf("pause history not closed by current owner: %+v", resumed.Pauses)
	}
	if err := expectPlanSpecimenConsistency(client, baseURL, plan.ID, specimen.ID, "active", "preparing"); err != nil {
		return err
	}

	// 恢复后进度可以继续写入；同一方案允许再次暂停与恢复，历史完整保留。
	if err := recordProgress(client, baseURL, plan.ID, 60, "矩阵剥离"); err != nil {
		return err
	}
	if _, err := pausePlan(client, baseURL, plan.ID,
		contracts.PausePlanRequest{Reason: "风刻笔检修", PausedBy: "keeper-zhou"},
		http.StatusOK); err != nil {
		return err
	}
	if _, err := resumePlan(client, baseURL, plan.ID,
		contracts.ResumePlanRequest{ResumedBy: "preparator-chen"},
		http.StatusOK); err != nil {
		return err
	}
	snapshot, err := getPlanSnapshot(client, baseURL, plan.ID)
	if err != nil {
		return err
	}
	if len(snapshot.Plan.Pauses) != 2 {
		return fmt.Errorf("expected two pause records, got %d", len(snapshot.Plan.Pauses))
	}
	if len(snapshot.Progress) != 3 {
		return fmt.Errorf("expected three progress entries after resume, got %d", len(snapshot.Progress))
	}

	if err := recordProgress(client, baseURL, plan.ID, 100, "表面加固"); err != nil {
		return err
	}
	return expectPlanSpecimenConsistency(client, baseURL, plan.ID, specimen.ID, "completed", "stabilized")
}

// 暂停期间任何进度写入都失败，且不会留下半截进度记录。
func checkProgressBlockedWhilePaused(client *http.Client, baseURL, catalog string) error {
	specimen, plan, err := activePlanSetup(client, baseURL, catalog)
	if err != nil {
		return err
	}
	if _, err := pausePlan(client, baseURL, plan.ID,
		contracts.PausePlanRequest{Reason: "责任人交接", PausedBy: "keeper-zhou"},
		http.StatusOK); err != nil {
		return err
	}
	if err := expectStatus(client, http.MethodPost, baseURL+"/plans/"+plan.ID+"/progress",
		contracts.ProgressRequest{
			Stage: "误写阶段", Percent: 40, Note: "应被拒绝", RecordedBy: "preparator-chen",
		}, http.StatusConflict, "plan_paused"); err != nil {
		return err
	}
	snapshot, err := getPlanSnapshot(client, baseURL, plan.ID)
	if err != nil {
		return err
	}
	if len(snapshot.Progress) != 1 {
		return fmt.Errorf("progress must not be appended while paused, got %d entries", len(snapshot.Progress))
	}
	if snapshot.Progress[0].Percent != 10 {
		return fmt.Errorf("rejected write must not alter existing progress: %+v", snapshot.Progress)
	}
	if snapshot.Plan.Status != "paused" {
		return fmt.Errorf("failed progress write must leave plan paused, got %s", snapshot.Plan.Status)
	}
	fetchedSpecimen, err := getSpecimen(client, baseURL, specimen.ID)
	if err != nil {
		return err
	}
	if fetchedSpecimen.Status != "on_hold" {
		return fmt.Errorf("failed progress write must leave specimen on_hold, got %s", fetchedSpecimen.Status)
	}
	return nil
}

// 重复暂停得到确定性冲突，状态与暂停记录数量保持不变。
func checkDoublePause(client *http.Client, baseURL, catalog string) error {
	_, plan, err := activePlanSetup(client, baseURL, catalog)
	if err != nil {
		return err
	}
	if _, err := pausePlan(client, baseURL, plan.ID,
		contracts.PausePlanRequest{Reason: "首次暂停", PausedBy: "keeper-zhou"},
		http.StatusOK); err != nil {
		return err
	}
	if err := expectStatus(client, http.MethodPost, baseURL+"/plans/"+plan.ID+"/pause",
		contracts.PausePlanRequest{Reason: "重复暂停", PausedBy: "keeper-zhou"},
		http.StatusConflict, "plan_already_paused"); err != nil {
		return err
	}
	snapshot, err := getPlanSnapshot(client, baseURL, plan.ID)
	if err != nil {
		return err
	}
	if snapshot.Plan.Status != "paused" || len(snapshot.Plan.Pauses) != 1 {
		return fmt.Errorf("double pause must be a no-op, got status %s pauses %d",
			snapshot.Plan.Status, len(snapshot.Plan.Pauses))
	}
	return nil
}

// 对活动方案重复恢复同样是确定性冲突，且不会改动历史。
func checkDoubleResume(client *http.Client, baseURL, catalog string) error {
	_, plan, err := activePlanSetup(client, baseURL, catalog)
	if err != nil {
		return err
	}
	if _, err := pausePlan(client, baseURL, plan.ID,
		contracts.PausePlanRequest{Reason: "设备检修", PausedBy: "keeper-zhou"},
		http.StatusOK); err != nil {
		return err
	}
	if _, err := resumePlan(client, baseURL, plan.ID,
		contracts.ResumePlanRequest{ResumedBy: "preparator-chen"},
		http.StatusOK); err != nil {
		return err
	}
	if err := expectStatus(client, http.MethodPost, baseURL+"/plans/"+plan.ID+"/resume",
		contracts.ResumePlanRequest{ResumedBy: "preparator-chen"},
		http.StatusConflict, "plan_not_paused"); err != nil {
		return err
	}
	snapshot, err := getPlanSnapshot(client, baseURL, plan.ID)
	if err != nil {
		return err
	}
	if snapshot.Plan.Status != "active" {
		return fmt.Errorf("double resume must leave plan active, got %s", snapshot.Plan.Status)
	}
	if snapshot.Plan.Pauses[0].ResumedAt == nil {
		return errors.New("closed pause record must keep its resume stamp")
	}
	return nil
}

// 草稿、已审批（尚无进度）和已完成方案都不能进入暂停。
func checkPauseRejectedByState(client *http.Client, baseURL, catalog string) error {
	specimen, err := registerSpecimenWithCatalog(client, baseURL, catalog)
	if err != nil {
		return err
	}
	plan, err := createPlan(client, baseURL, specimen.ID)
	if err != nil {
		return err
	}
	if err := expectStatus(client, http.MethodPost, baseURL+"/plans/"+plan.ID+"/pause",
		contracts.PausePlanRequest{Reason: "草稿上非法暂停", PausedBy: "keeper-zhou"},
		http.StatusConflict, "plan_not_pausable"); err != nil {
		return err
	}
	if err := requestJSON(client, http.MethodPost, baseURL+"/plans/"+plan.ID+"/approval",
		contracts.ApprovalRequest{ApprovedBy: "curator-lin"}, http.StatusOK, &planHoldView{}); err != nil {
		return err
	}
	if err := expectStatus(client, http.MethodPost, baseURL+"/plans/"+plan.ID+"/pause",
		contracts.PausePlanRequest{Reason: "已审批但未开始", PausedBy: "keeper-zhou"},
		http.StatusConflict, "plan_not_pausable"); err != nil {
		return err
	}

	_, completed, err := completedPlanSetupWithCatalog(client, baseURL, catalog+"-DONE")
	if err != nil {
		return err
	}
	if err := recordProgress(client, baseURL, completed.ID, 100, "表面加固"); err != nil {
		return err
	}
	if err := expectStatus(client, http.MethodPost, baseURL+"/plans/"+completed.ID+"/pause",
		contracts.PausePlanRequest{Reason: "完成后非法暂停", PausedBy: "keeper-zhou"},
		http.StatusConflict, "plan_not_pausable"); err != nil {
		return err
	}
	return nil
}

// 在活动方案（未暂停）上恢复被拒绝；不存在的方案返回 404。
func checkResumeRejectedByState(client *http.Client, baseURL, catalog string) error {
	_, plan, err := activePlanSetup(client, baseURL, catalog)
	if err != nil {
		return err
	}
	if err := expectStatus(client, http.MethodPost, baseURL+"/plans/"+plan.ID+"/resume",
		contracts.ResumePlanRequest{ResumedBy: "preparator-chen"},
		http.StatusConflict, "plan_not_paused"); err != nil {
		return err
	}
	if err := expectStatus(client, http.MethodPost, baseURL+"/plans/plan-missing/resume",
		contracts.ResumePlanRequest{ResumedBy: "preparator-chen"},
		http.StatusNotFound, "plan_not_found"); err != nil {
		return err
	}
	return nil
}

// 暂停请求必须带原因和责任人，非法输入返回 400 且不改动任何状态。
func checkInvalidPauseRequest(client *http.Client, baseURL, catalog string) error {
	specimen, plan, err := activePlanSetup(client, baseURL, catalog)
	if err != nil {
		return err
	}
	if err := expectStatus(client, http.MethodPost, baseURL+"/plans/"+plan.ID+"/pause",
		contracts.PausePlanRequest{Reason: "   ", PausedBy: "keeper-zhou"},
		http.StatusBadRequest, "required_field"); err != nil {
		return err
	}
	if err := expectStatus(client, http.MethodPost, baseURL+"/plans/"+plan.ID+"/pause",
		contracts.PausePlanRequest{Reason: "展柜借用", PausedBy: ""},
		http.StatusBadRequest, "required_field"); err != nil {
		return err
	}
	return expectPlanSpecimenConsistency(client, baseURL, plan.ID, specimen.ID, "active", "preparing")
}

// 并发暂停：恰有一个成功，其余全部冲突，最终只留下一条暂停记录。
func checkConcurrentPauses(client *http.Client, baseURL, catalog string) error {
	_, plan, err := activePlanSetup(client, baseURL, catalog)
	if err != nil {
		return err
	}
	statuses, codes := doConcurrent(8, func() (int, string) {
		return postStatus(client, baseURL+"/plans/"+plan.ID+"/pause",
			contracts.PausePlanRequest{Reason: "并发暂停", PausedBy: "keeper-zhou"})
	})
	if countStatus(statuses, http.StatusOK) != 1 || countStatus(statuses, http.StatusConflict) != 7 {
		return fmt.Errorf("concurrent pause needs exactly one success: %v", statuses)
	}
	for i, code := range codes {
		want := "plan_already_paused"
		if statuses[i] == http.StatusOK {
			want = ""
		}
		if code != want {
			return fmt.Errorf("unexpected concurrent pause error code %q (status %d)", code, statuses[i])
		}
	}
	snapshot, err := getPlanSnapshot(client, baseURL, plan.ID)
	if err != nil {
		return err
	}
	if snapshot.Plan.Status != "paused" || len(snapshot.Plan.Pauses) != 1 {
		return fmt.Errorf("concurrent pause left state %s with %d records",
			snapshot.Plan.Status, len(snapshot.Plan.Pauses))
	}
	return nil
}

// 并发恢复：恰有一个成功，其余全部冲突，暂停记录只闭合一次。
func checkConcurrentResumes(client *http.Client, baseURL, catalog string) error {
	_, plan, err := activePlanSetup(client, baseURL, catalog)
	if err != nil {
		return err
	}
	if _, err := pausePlan(client, baseURL, plan.ID,
		contracts.PausePlanRequest{Reason: "展柜借用", PausedBy: "keeper-zhou"},
		http.StatusOK); err != nil {
		return err
	}
	statuses, codes := doConcurrent(8, func() (int, string) {
		return postStatus(client, baseURL+"/plans/"+plan.ID+"/resume",
			contracts.ResumePlanRequest{ResumedBy: "preparator-chen"})
	})
	if countStatus(statuses, http.StatusOK) != 1 || countStatus(statuses, http.StatusConflict) != 7 {
		return fmt.Errorf("concurrent resume needs exactly one success: %v", statuses)
	}
	for i, code := range codes {
		want := "plan_not_paused"
		if statuses[i] == http.StatusOK {
			want = ""
		}
		if code != want {
			return fmt.Errorf("unexpected concurrent resume error code %q (status %d)", code, statuses[i])
		}
	}
	snapshot, err := getPlanSnapshot(client, baseURL, plan.ID)
	if err != nil {
		return err
	}
	if snapshot.Plan.Status != "active" || len(snapshot.Plan.Pauses) != 1 ||
		snapshot.Plan.Pauses[0].ResumedAt == nil {
		return fmt.Errorf("concurrent resume left inconsistent state: %+v", snapshot.Plan)
	}
	return nil
}

// 暂停期间并发写进度：全部冲突，进度记录一条都不能新增，状态保持暂停。
func checkConcurrentProgressWhilePaused(client *http.Client, baseURL, catalog string) error {
	specimen, plan, err := activePlanSetup(client, baseURL, catalog)
	if err != nil {
		return err
	}
	if _, err := pausePlan(client, baseURL, plan.ID,
		contracts.PausePlanRequest{Reason: "设备检修", PausedBy: "keeper-zhou"},
		http.StatusOK); err != nil {
		return err
	}
	statuses, codes := doConcurrent(8, func() (int, string) {
		return postStatus(client, baseURL+"/plans/"+plan.ID+"/progress",
			contracts.ProgressRequest{
				Stage: "并发误写", Percent: 50, Note: "应全部失败", RecordedBy: "preparator-chen",
			})
	})
	if countStatus(statuses, http.StatusConflict) != 8 {
		return fmt.Errorf("all paused progress writes must conflict: %v", statuses)
	}
	for _, code := range codes {
		if code != "plan_paused" {
			return fmt.Errorf("paused progress must report plan_paused, got %q", code)
		}
	}
	snapshot, err := getPlanSnapshot(client, baseURL, plan.ID)
	if err != nil {
		return err
	}
	if len(snapshot.Progress) != 1 || snapshot.Plan.Status != "paused" {
		return fmt.Errorf("paused state changed by rejected writes: status=%s progress=%d",
			snapshot.Plan.Status, len(snapshot.Progress))
	}
	fetchedSpecimen, err := getSpecimen(client, baseURL, specimen.ID)
	if err != nil {
		return err
	}
	if fetchedSpecimen.Status != "on_hold" {
		return fmt.Errorf("specimen left inconsistent after rejected writes: %s", fetchedSpecimen.Status)
	}
	return nil
}

// 暂停与进度并发：无论谁先获得事务，最终状态必须自洽——
// 暂停先到则进度被拒（paused + on_hold + 无新记录）；
// 进度先到则暂停成功（paused + on_hold + 恰好新增一条记录）。
func checkConcurrentPauseVsProgress(client *http.Client, baseURL, catalog string) error {
	specimen, plan, err := activePlanSetup(client, baseURL, catalog)
	if err != nil {
		return err
	}
	start := make(chan struct{})
	var pauseStatus, progressStatus int
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		pauseStatus, _ = postStatus(client, baseURL+"/plans/"+plan.ID+"/pause",
			contracts.PausePlanRequest{Reason: "与进度并发", PausedBy: "keeper-zhou"})
	}()
	go func() {
		defer wg.Done()
		<-start
		progressStatus, _ = postStatus(client, baseURL+"/plans/"+plan.ID+"/progress",
			contracts.ProgressRequest{
				Stage: "与暂停并发", Percent: 50, Note: "竞争请求", RecordedBy: "preparator-chen",
			})
	}()
	close(start)
	wg.Wait()

	if pauseStatus != http.StatusOK {
		return fmt.Errorf("pause request in race must succeed, got %d", pauseStatus)
	}
	if progressStatus != http.StatusCreated && progressStatus != http.StatusConflict {
		return fmt.Errorf("racing progress must be created or rejected, got %d", progressStatus)
	}
	snapshot, err := getPlanSnapshot(client, baseURL, plan.ID)
	if err != nil {
		return err
	}
	if snapshot.Plan.Status != "paused" || len(snapshot.Plan.Pauses) != 1 {
		return fmt.Errorf("race must end paused with one record, got %s pauses=%d",
			snapshot.Plan.Status, len(snapshot.Plan.Pauses))
	}
	wantEntries := 1
	if progressStatus == http.StatusCreated {
		wantEntries = 2
	}
	if len(snapshot.Progress) != wantEntries {
		return fmt.Errorf("racing result inconsistent: progress status %d entries %d",
			progressStatus, len(snapshot.Progress))
	}
	fetchedSpecimen, err := getSpecimen(client, baseURL, specimen.ID)
	if err != nil {
		return err
	}
	if fetchedSpecimen.Status != "on_hold" {
		return fmt.Errorf("specimen must be on_hold after pause race, got %s", fetchedSpecimen.Status)
	}
	return nil
}

// 恢复与进度并发：恢复先到则进度成功（active + preparing + 新记录）；
// 进度先到则被拒（仍 paused + on_hold + 无新记录）。两种结局都必须自洽。
func checkConcurrentResumeVsProgress(client *http.Client, baseURL, catalog string) error {
	specimen, plan, err := activePlanSetup(client, baseURL, catalog)
	if err != nil {
		return err
	}
	if _, err := pausePlan(client, baseURL, plan.ID,
		contracts.PausePlanRequest{Reason: "恢复竞赛前暂停", PausedBy: "keeper-zhou"},
		http.StatusOK); err != nil {
		return err
	}
	start := make(chan struct{})
	var resumeStatus, progressStatus int
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		resumeStatus, _ = postStatus(client, baseURL+"/plans/"+plan.ID+"/resume",
			contracts.ResumePlanRequest{ResumedBy: "preparator-chen"})
	}()
	go func() {
		defer wg.Done()
		<-start
		progressStatus, _ = postStatus(client, baseURL+"/plans/"+plan.ID+"/progress",
			contracts.ProgressRequest{
				Stage: "与恢复并发", Percent: 55, Note: "竞争请求", RecordedBy: "preparator-chen",
			})
	}()
	close(start)
	wg.Wait()

	if resumeStatus != http.StatusOK {
		return fmt.Errorf("resume request in race must succeed, got %d", resumeStatus)
	}
	if progressStatus != http.StatusCreated && progressStatus != http.StatusConflict {
		return fmt.Errorf("racing progress must be created or rejected, got %d", progressStatus)
	}
	snapshot, err := getPlanSnapshot(client, baseURL, plan.ID)
	if err != nil {
		return err
	}
	if snapshot.Plan.Status != "active" || len(snapshot.Plan.Pauses) != 1 ||
		snapshot.Plan.Pauses[0].ResumedAt == nil {
		return fmt.Errorf("race must end active with closed pause, got %+v", snapshot.Plan)
	}
	wantEntries := 1
	if progressStatus == http.StatusCreated {
		wantEntries = 2
	}
	if len(snapshot.Progress) != wantEntries {
		return fmt.Errorf("racing result inconsistent: progress status %d entries %d",
			progressStatus, len(snapshot.Progress))
	}
	fetchedSpecimen, err := getSpecimen(client, baseURL, specimen.ID)
	if err != nil {
		return err
	}
	if fetchedSpecimen.Status != "preparing" {
		return fmt.Errorf("specimen must be preparing after resume race, got %s", fetchedSpecimen.Status)
	}
	return nil
}

func activePlanSetup(client *http.Client, baseURL, catalog string) (specimenView, planView, error) {
	specimen, err := registerSpecimenWithCatalog(client, baseURL, catalog)
	if err != nil {
		return specimenView{}, planView{}, err
	}
	plan, err := createPlan(client, baseURL, specimen.ID)
	if err != nil {
		return specimenView{}, planView{}, err
	}
	if err := requestJSON(client, http.MethodPost, baseURL+"/plans/"+plan.ID+"/approval",
		contracts.ApprovalRequest{ApprovedBy: "curator-lin"}, http.StatusOK, &planView{}); err != nil {
		return specimenView{}, planView{}, err
	}
	// 第一条进度把已审批方案推进为活动方案，暂停只允许发生在活动态。
	if err := recordProgress(client, baseURL, plan.ID, 10, "开工记录"); err != nil {
		return specimenView{}, planView{}, err
	}
	return specimen, plan, nil
}

func completedPlanSetupWithCatalog(client *http.Client, baseURL, catalog string) (specimenView, planView, error) {
	specimen, err := registerSpecimenWithCatalog(client, baseURL, catalog)
	if err != nil {
		return specimenView{}, planView{}, err
	}
	plan, err := createPlan(client, baseURL, specimen.ID)
	if err != nil {
		return specimenView{}, planView{}, err
	}
	var approved planView
	if err := requestJSON(client, http.MethodPost, baseURL+"/plans/"+plan.ID+"/approval",
		contracts.ApprovalRequest{ApprovedBy: "curator-lin"}, http.StatusOK, &approved); err != nil {
		return specimenView{}, planView{}, err
	}
	return specimen, approved, nil
}

func pausePlan(client *http.Client, baseURL, planID string, request contracts.PausePlanRequest, want int) (planHoldView, error) {
	var value planHoldView
	err := requestJSON(client, http.MethodPost, baseURL+"/plans/"+planID+"/pause", request, want, &value)
	return value, err
}

func resumePlan(client *http.Client, baseURL, planID string, request contracts.ResumePlanRequest, want int) (planHoldView, error) {
	var value planHoldView
	err := requestJSON(client, http.MethodPost, baseURL+"/plans/"+planID+"/resume", request, want, &value)
	return value, err
}

func getPlanSnapshot(client *http.Client, baseURL, planID string) (planHoldSnapshot, error) {
	var snapshot planHoldSnapshot
	err := requestJSON(client, http.MethodGet, baseURL+"/plans/"+planID, nil, http.StatusOK, &snapshot)
	return snapshot, err
}

func getSpecimen(client *http.Client, baseURL, specimenID string) (specimenView, error) {
	var value specimenView
	err := requestJSON(client, http.MethodGet, baseURL+"/specimens/"+specimenID, nil, http.StatusOK, &value)
	return value, err
}

func expectPlanSpecimenConsistency(
	client *http.Client, baseURL, planID, specimenID, planStatus, specimenStatus string,
) error {
	snapshot, err := getPlanSnapshot(client, baseURL, planID)
	if err != nil {
		return err
	}
	if snapshot.Plan.Status != planStatus {
		return fmt.Errorf("plan status mismatch: want %s got %s", planStatus, snapshot.Plan.Status)
	}
	specimen, err := getSpecimen(client, baseURL, specimenID)
	if err != nil {
		return err
	}
	if specimen.Status != specimenStatus {
		return fmt.Errorf("specimen status mismatch: want %s got %s", specimenStatus, specimen.Status)
	}
	return nil
}

func expectStatus(client *http.Client, method, url string, body any, wantStatus int, wantCode string) error {
	status, code, err := requestStatus(client, method, url, body)
	if err != nil {
		return err
	}
	if status != wantStatus {
		return fmt.Errorf("%s %s returned %d, want %d (code=%s)", method, url, status, wantStatus, code)
	}
	if wantCode != "" && code != wantCode {
		return fmt.Errorf("%s %s returned code %q, want %q", method, url, code, wantCode)
	}
	return nil
}

func doConcurrent(times int, action func() (int, string)) ([]int, []string) {
	var wg sync.WaitGroup
	statuses := make([]int, times)
	codes := make([]string, times)
	start := make(chan struct{})
	for i := 0; i < times; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			statuses[index], codes[index] = action()
		}(i)
	}
	close(start)
	wg.Wait()
	return statuses, codes
}

func countStatus(values []int, target int) int {
	total := 0
	for _, value := range values {
		if value == target {
			total++
		}
	}
	return total
}
