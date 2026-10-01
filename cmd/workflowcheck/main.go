package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"time"

	"example.com/paleo-prep-station/internal/contracts"
	"example.com/paleo-prep-station/internal/httpapi"
	"example.com/paleo-prep-station/internal/repository"
	"example.com/paleo-prep-station/internal/station"
)

type specimenView struct {
	ID            string `json:"id"`
	CatalogNumber string `json:"catalog_number"`
	Status        string `json:"status"`
}

type planView struct {
	ID         string     `json:"id"`
	SpecimenID string     `json:"specimen_id"`
	Status     string     `json:"status"`
	HoldReason string     `json:"hold_reason"`
	HoldOwner  string     `json:"hold_owner"`
	HoldAt     *time.Time `json:"hold_at"`
	ResumedBy  string     `json:"resumed_by"`
	ResumedAt  *time.Time `json:"resumed_at"`
}

type planSnapshot struct {
	Plan     planView `json:"plan"`
	Progress []struct {
		Sequence int `json:"sequence"`
		Percent  int `json:"percent"`
	} `json:"progress"`
}

type releaseView struct {
	ID         string `json:"id"`
	SpecimenID string `json:"specimen_id"`
}

type errorView struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func main() {
	workflow := flag.String("workflow", "", "workflow id to check")
	flag.Parse()

	if err := run(*workflow); err != nil {
		fmt.Fprintln(os.Stderr, "workflow check failed:", err)
		os.Exit(1)
	}
	fmt.Printf("workflow check passed: %s\n", *workflow)
}

func run(workflow string) error {
	handler := httpapi.NewRouter(station.New(repository.NewMemory()))
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}

	switch workflow {
	case "register-specimen":
		specimen, err := registerSpecimen(client, server.URL)
		if err != nil {
			return err
		}
		if specimen.Status != "registered" {
			return fmt.Errorf("expected registered status, got %s", specimen.Status)
		}
		var fetched specimenView
		if err := requestJSON(client, http.MethodGet, server.URL+"/specimens/"+specimen.ID, nil, http.StatusOK, &fetched); err != nil {
			return err
		}
		if fetched.ID != specimen.ID {
			return errors.New("registered specimen cannot be read back")
		}
		return nil

	case "approve-prep-plan":
		specimen, err := registerSpecimen(client, server.URL)
		if err != nil {
			return err
		}
		plan, err := createPlan(client, server.URL, specimen.ID)
		if err != nil {
			return err
		}
		var approved planView
		if err := requestJSON(
			client,
			http.MethodPost,
			server.URL+"/plans/"+plan.ID+"/approval",
			contracts.ApprovalRequest{ApprovedBy: "curator-lin"},
			http.StatusOK,
			&approved,
		); err != nil {
			return err
		}
		if approved.Status != "approved" {
			return fmt.Errorf("expected approved plan, got %s", approved.Status)
		}
		var fetchedSpecimen specimenView
		if err := requestJSON(client, http.MethodGet, server.URL+"/specimens/"+specimen.ID, nil, http.StatusOK, &fetchedSpecimen); err != nil {
			return err
		}
		if fetchedSpecimen.Status != "preparing" {
			return fmt.Errorf("expected preparing specimen, got %s", fetchedSpecimen.Status)
		}
		return nil

	case "log-preparation":
		specimen, plan, err := completedPlanSetup(client, server.URL)
		if err != nil {
			return err
		}
		if err := recordProgress(client, server.URL, plan.ID, 35, "矩阵剥离"); err != nil {
			return err
		}
		if err := recordProgress(client, server.URL, plan.ID, 100, "表面加固"); err != nil {
			return err
		}
		var snapshot planSnapshot
		if err := requestJSON(client, http.MethodGet, server.URL+"/plans/"+plan.ID, nil, http.StatusOK, &snapshot); err != nil {
			return err
		}
		if snapshot.Plan.Status != "completed" || len(snapshot.Progress) != 2 {
			return errors.New("plan did not complete with exactly two progress entries")
		}
		var fetchedSpecimen specimenView
		if err := requestJSON(client, http.MethodGet, server.URL+"/specimens/"+specimen.ID, nil, http.StatusOK, &fetchedSpecimen); err != nil {
			return err
		}
		if fetchedSpecimen.Status != "stabilized" {
			return fmt.Errorf("expected stabilized specimen, got %s", fetchedSpecimen.Status)
		}
		return nil

	case "hold-and-resume":
		return checkHoldAndResume(client, server.URL)

	case "release-for-study":
		specimen, plan, err := completedPlanSetup(client, server.URL)
		if err != nil {
			return err
		}
		if err := recordProgress(client, server.URL, plan.ID, 100, "表面加固"); err != nil {
			return err
		}
		var release releaseView
		if err := requestJSON(
			client,
			http.MethodPost,
			server.URL+"/specimens/"+specimen.ID+"/release",
			contracts.ReleaseRequest{
				Purpose:    "比较解剖研究",
				Recipient:  "地层研究组",
				ApprovedBy: "keeper-zhou",
			},
			http.StatusCreated,
			&release,
		); err != nil {
			return err
		}
		if release.SpecimenID != specimen.ID {
			return errors.New("release record references the wrong specimen")
		}
		var fetched specimenView
		if err := requestJSON(client, http.MethodGet, server.URL+"/specimens/"+specimen.ID, nil, http.StatusOK, &fetched); err != nil {
			return err
		}
		if fetched.Status != "available" {
			return fmt.Errorf("expected available specimen, got %s", fetched.Status)
		}
		return nil
	default:
		return fmt.Errorf("unknown workflow %q", workflow)
	}
}

func checkHoldAndResume(client *http.Client, baseURL string) error {
	specimen, plan, err := completedPlanSetup(client, baseURL)
	if err != nil {
		return err
	}
	planURL := baseURL + "/plans/" + plan.ID
	holdURL := planURL + "/hold"
	resumeURL := planURL + "/resume"
	progressURL := planURL + "/progress"

	// 已审批但尚未开始制备的方案不能暂停。
	var conflict errorView
	if err := requestJSON(
		client,
		http.MethodPost,
		holdURL,
		contracts.HoldRequest{Reason: "展柜借用", Owner: "preparator-chen"},
		http.StatusConflict,
		&conflict,
	); err != nil {
		return err
	}
	if conflict.Code != "plan_not_pausable" {
		return fmt.Errorf("expected plan_not_pausable for approved plan, got %s", conflict.Code)
	}

	if err := recordProgress(client, baseURL, plan.ID, 30, "基质粗剥离"); err != nil {
		return err
	}

	// 暂停必须携带原因和责任人。
	var invalid errorView
	if err := requestJSON(
		client,
		http.MethodPost,
		holdURL,
		contracts.HoldRequest{Reason: " ", Owner: "preparator-chen"},
		http.StatusBadRequest,
		&invalid,
	); err != nil {
		return err
	}
	if invalid.Code != "required_field" {
		return fmt.Errorf("expected required_field for empty hold reason, got %s", invalid.Code)
	}

	var held planView
	if err := requestJSON(
		client,
		http.MethodPost,
		holdURL,
		contracts.HoldRequest{Reason: "展柜借用", Owner: "preparator-chen"},
		http.StatusOK,
		&held,
	); err != nil {
		return err
	}
	if held.Status != "paused" || held.HoldReason != "展柜借用" || held.HoldOwner != "preparator-chen" || held.HoldAt == nil {
		return fmt.Errorf("hold did not record reason, owner and time: %+v", held)
	}

	// 暂停在方案与标本的公开读取中保持一致。
	var snapshot planSnapshot
	if err := requestJSON(client, http.MethodGet, planURL, nil, http.StatusOK, &snapshot); err != nil {
		return err
	}
	if snapshot.Plan.Status != "paused" || snapshot.Plan.HoldOwner != "preparator-chen" || len(snapshot.Progress) != 1 {
		return fmt.Errorf("paused plan snapshot inconsistent: %+v", snapshot)
	}
	var fetched specimenView
	if err := requestJSON(client, http.MethodGet, baseURL+"/specimens/"+specimen.ID, nil, http.StatusOK, &fetched); err != nil {
		return err
	}
	if fetched.Status != "on_hold" {
		return fmt.Errorf("expected on_hold specimen, got %s", fetched.Status)
	}

	// 暂停期间进度写入必须失败且不留下记录。
	var blocked errorView
	if err := requestJSON(
		client,
		http.MethodPost,
		progressURL,
		contracts.ProgressRequest{Stage: "继续剥离", Percent: 45, Note: "暂停期间误写", RecordedBy: "preparator-chen"},
		http.StatusConflict,
		&blocked,
	); err != nil {
		return err
	}
	if blocked.Code != "plan_on_hold" {
		return fmt.Errorf("expected plan_on_hold for progress during hold, got %s", blocked.Code)
	}
	if err := requestJSON(client, http.MethodGet, planURL, nil, http.StatusOK, &snapshot); err != nil {
		return err
	}
	if len(snapshot.Progress) != 1 {
		return fmt.Errorf("progress written during hold: %+v", snapshot.Progress)
	}

	// 重复暂停返回确定的冲突。
	if err := requestJSON(
		client,
		http.MethodPost,
		holdURL,
		contracts.HoldRequest{Reason: "设备检修", Owner: "preparator-chen"},
		http.StatusConflict,
		&conflict,
	); err != nil {
		return err
	}
	if conflict.Code != "plan_not_pausable" {
		return fmt.Errorf("expected plan_not_pausable for duplicate hold, got %s", conflict.Code)
	}

	var resumed planView
	if err := requestJSON(
		client,
		http.MethodPost,
		resumeURL,
		contracts.ResumeRequest{ResumedBy: "preparator-chen"},
		http.StatusOK,
		&resumed,
	); err != nil {
		return err
	}
	if resumed.Status != "active" || resumed.ResumedBy != "preparator-chen" || resumed.ResumedAt == nil {
		return fmt.Errorf("resume did not restore active plan: %+v", resumed)
	}
	if err := requestJSON(client, http.MethodGet, baseURL+"/specimens/"+specimen.ID, nil, http.StatusOK, &fetched); err != nil {
		return err
	}
	if fetched.Status != "preparing" {
		return fmt.Errorf("expected preparing specimen after resume, got %s", fetched.Status)
	}

	// 重复恢复返回确定的冲突。
	if err := requestJSON(
		client,
		http.MethodPost,
		resumeURL,
		contracts.ResumeRequest{ResumedBy: "preparator-chen"},
		http.StatusConflict,
		&conflict,
	); err != nil {
		return err
	}
	if conflict.Code != "plan_not_paused" {
		return fmt.Errorf("expected plan_not_paused for duplicate resume, got %s", conflict.Code)
	}

	if err := recordProgress(client, baseURL, plan.ID, 60, "细部清理"); err != nil {
		return err
	}

	// 并发暂停与进度请求必须有确定结果且不留下半截状态。
	if err := checkConcurrentHoldAndProgress(client, baseURL, specimen.ID, plan.ID); err != nil {
		return err
	}

	// 再次恢复后方案可以走完剩余进度。
	var again planView
	if err := requestJSON(
		client,
		http.MethodPost,
		resumeURL,
		contracts.ResumeRequest{ResumedBy: "preparator-chen"},
		http.StatusOK,
		&again,
	); err != nil {
		return err
	}
	if again.Status != "active" {
		return fmt.Errorf("expected active plan after second resume, got %s", again.Status)
	}
	if err := requestJSON(client, http.MethodGet, baseURL+"/specimens/"+specimen.ID, nil, http.StatusOK, &fetched); err != nil {
		return err
	}
	if fetched.Status != "preparing" {
		return fmt.Errorf("expected preparing specimen after second resume, got %s", fetched.Status)
	}
	if err := recordProgress(client, baseURL, plan.ID, 100, "表面加固"); err != nil {
		return err
	}
	if err := requestJSON(client, http.MethodGet, planURL, nil, http.StatusOK, &snapshot); err != nil {
		return err
	}
	if snapshot.Plan.Status != "completed" {
		return fmt.Errorf("expected completed plan, got %s", snapshot.Plan.Status)
	}

	// 已完成的方案不能再暂停或恢复。
	if err := requestJSON(
		client,
		http.MethodPost,
		holdURL,
		contracts.HoldRequest{Reason: "展柜借用", Owner: "preparator-chen"},
		http.StatusConflict,
		&conflict,
	); err != nil {
		return err
	}
	if conflict.Code != "plan_not_pausable" {
		return fmt.Errorf("expected plan_not_pausable for completed plan, got %s", conflict.Code)
	}
	if err := requestJSON(
		client,
		http.MethodPost,
		resumeURL,
		contracts.ResumeRequest{ResumedBy: "preparator-chen"},
		http.StatusConflict,
		&conflict,
	); err != nil {
		return err
	}
	if conflict.Code != "plan_not_paused" {
		return fmt.Errorf("expected plan_not_paused for completed plan, got %s", conflict.Code)
	}
	return nil
}

func checkConcurrentHoldAndProgress(client *http.Client, baseURL, specimenID, planID string) error {
	type outcome struct {
		status int
		body   []byte
	}
	holdCh := make(chan outcome, 1)
	progressCh := make(chan outcome, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		status, body, err := doJSON(
			client,
			http.MethodPost,
			baseURL+"/plans/"+planID+"/hold",
			contracts.HoldRequest{Reason: "设备检修", Owner: "preparator-chen"},
		)
		if err != nil {
			holdCh <- outcome{status: -1, body: []byte(err.Error())}
			return
		}
		holdCh <- outcome{status: status, body: body}
	}()
	go func() {
		defer wg.Done()
		status, body, err := doJSON(
			client,
			http.MethodPost,
			baseURL+"/plans/"+planID+"/progress",
			contracts.ProgressRequest{Stage: "围岩清理", Percent: 80, Note: "并发检查", RecordedBy: "preparator-chen"},
		)
		if err != nil {
			progressCh <- outcome{status: -1, body: []byte(err.Error())}
			return
		}
		progressCh <- outcome{status: status, body: body}
	}()
	wg.Wait()
	hold := <-holdCh
	progress := <-progressCh

	if hold.status != http.StatusOK {
		return fmt.Errorf("concurrent hold returned %d: %s", hold.status, string(hold.body))
	}
	if progress.status != http.StatusCreated && progress.status != http.StatusConflict {
		return fmt.Errorf("concurrent progress returned %d: %s", progress.status, string(progress.body))
	}
	if progress.status == http.StatusConflict {
		var blocked errorView
		if err := json.Unmarshal(progress.body, &blocked); err != nil {
			return fmt.Errorf("decode concurrent progress conflict: %w", err)
		}
		if blocked.Code != "plan_on_hold" {
			return fmt.Errorf("expected plan_on_hold for losing progress, got %s", blocked.Code)
		}
	}

	var snapshot planSnapshot
	if err := requestJSON(client, http.MethodGet, baseURL+"/plans/"+planID, nil, http.StatusOK, &snapshot); err != nil {
		return err
	}
	if snapshot.Plan.Status != "paused" || snapshot.Plan.HoldReason != "设备检修" {
		return fmt.Errorf("concurrent hold left plan %s with reason %q", snapshot.Plan.Status, snapshot.Plan.HoldReason)
	}
	wantEntries := 2
	if progress.status == http.StatusCreated {
		wantEntries = 3
	}
	if len(snapshot.Progress) != wantEntries {
		return fmt.Errorf("expected %d progress entries after race, got %d", wantEntries, len(snapshot.Progress))
	}
	for i, entry := range snapshot.Progress {
		if entry.Sequence != i+1 {
			return fmt.Errorf("progress sequence gap at index %d: %+v", i, snapshot.Progress)
		}
		if i > 0 && entry.Percent < snapshot.Progress[i-1].Percent {
			return fmt.Errorf("progress regressed after race: %+v", snapshot.Progress)
		}
	}
	if progress.status == http.StatusCreated && snapshot.Progress[len(snapshot.Progress)-1].Percent != 80 {
		return fmt.Errorf("winning progress entry missing: %+v", snapshot.Progress)
	}

	var fetched specimenView
	if err := requestJSON(client, http.MethodGet, baseURL+"/specimens/"+specimenID, nil, http.StatusOK, &fetched); err != nil {
		return err
	}
	if fetched.Status != "on_hold" {
		return fmt.Errorf("expected on_hold specimen after concurrent hold, got %s", fetched.Status)
	}
	return nil
}

func registerSpecimen(client *http.Client, baseURL string) (specimenView, error) {
	var specimen specimenView
	err := requestJSON(
		client,
		http.MethodPost,
		baseURL+"/specimens",
		contracts.RegisterSpecimenRequest{
			CatalogNumber: "PPS-2026-001",
			FieldLabel:    "北坡-07",
			TaxonHint:     "大型脊椎动物",
			MatrixKind:    "粉砂岩",
			JacketNotes:   "夹克边角完好",
		},
		http.StatusCreated,
		&specimen,
	)
	return specimen, err
}

func createPlan(client *http.Client, baseURL, specimenID string) (planView, error) {
	var plan planView
	err := requestJSON(
		client,
		http.MethodPost,
		baseURL+"/specimens/"+specimenID+"/plans",
		contracts.CreatePlanRequest{
			Objective:      "暴露主要关节面",
			Method:         "手工显微剥离",
			Adhesive:       "可逆树脂",
			TargetExposure: 80,
		},
		http.StatusCreated,
		&plan,
	)
	return plan, err
}

func completedPlanSetup(client *http.Client, baseURL string) (specimenView, planView, error) {
	specimen, err := registerSpecimen(client, baseURL)
	if err != nil {
		return specimenView{}, planView{}, err
	}
	plan, err := createPlan(client, baseURL, specimen.ID)
	if err != nil {
		return specimenView{}, planView{}, err
	}
	var approved planView
	if err := requestJSON(
		client,
		http.MethodPost,
		baseURL+"/plans/"+plan.ID+"/approval",
		contracts.ApprovalRequest{ApprovedBy: "curator-lin"},
		http.StatusOK,
		&approved,
	); err != nil {
		return specimenView{}, planView{}, err
	}
	return specimen, approved, nil
}

func recordProgress(client *http.Client, baseURL, planID string, percent int, stage string) error {
	var ignored struct{}
	return requestJSON(
		client,
		http.MethodPost,
		baseURL+"/plans/"+planID+"/progress",
		contracts.ProgressRequest{
			Stage:      stage,
			Percent:    percent,
			Note:       "本地链路检查",
			RecordedBy: "preparator-chen",
		},
		http.StatusCreated,
		&ignored,
	)
}

func requestJSON(
	client *http.Client,
	method string,
	url string,
	body any,
	wantStatus int,
	target any,
) error {
	status, raw, err := doJSON(client, method, url, body)
	if err != nil {
		return err
	}
	if status != wantStatus {
		return fmt.Errorf("request %s %s returned %d: %s", method, url, status, string(raw))
	}
	if target == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode response for %s %s: %w", method, url, err)
	}
	return nil
}

func doJSON(client *http.Client, method string, url string, body any) (int, []byte, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, payload)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return response.StatusCode, raw, nil
}
