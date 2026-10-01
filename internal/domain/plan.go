package domain

import (
	"time"

	"example.com/paleo-prep-station/internal/contracts"
)

type PlanStatus string

const (
	PlanDraft     PlanStatus = "draft"
	PlanApproved  PlanStatus = "approved"
	PlanActive    PlanStatus = "active"
	PlanPaused    PlanStatus = "paused"
	PlanCompleted PlanStatus = "completed"
	PlanCancelled PlanStatus = "cancelled"
)

// PlanPause 记录一次制备暂停及其恢复；ResumedAt 为空表示当前仍在暂停。
type PlanPause struct {
	Reason    string     `json:"reason"`
	PausedBy  string     `json:"paused_by"`
	PausedAt  time.Time  `json:"paused_at"`
	ResumedBy string     `json:"resumed_by,omitempty"`
	ResumedAt *time.Time `json:"resumed_at,omitempty"`
}

type PrepPlan struct {
	ID             string      `json:"id"`
	SpecimenID     string      `json:"specimen_id"`
	Objective      string      `json:"objective"`
	Method         string      `json:"method"`
	Adhesive       string      `json:"adhesive"`
	TargetExposure int         `json:"target_exposure"`
	Status         PlanStatus  `json:"status"`
	ApprovedBy     string      `json:"approved_by,omitempty"`
	ApprovedAt     *time.Time  `json:"approved_at,omitempty"`
	Pauses         []PlanPause `json:"pauses,omitempty"`
	Version        int         `json:"version"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

func NewPrepPlan(
	id string,
	specimen Specimen,
	objective string,
	method string,
	adhesive string,
	targetExposure int,
	now time.Time,
) (PrepPlan, error) {
	if id == "" {
		return PrepPlan{}, contracts.Internal("missing_generated_id", "方案标识未生成", nil)
	}
	if !specimen.CanCreatePlan() {
		return PrepPlan{}, contracts.Conflict("specimen_not_plannable", "当前标本状态不能创建修复方案")
	}
	goal, err := requiredText("修复目标", objective, 200)
	if err != nil {
		return PrepPlan{}, err
	}
	approach, err := requiredText("修复方法", method, 160)
	if err != nil {
		return PrepPlan{}, err
	}
	adhesiveValue, err := optionalText("黏结材料", adhesive, 120)
	if err != nil {
		return PrepPlan{}, err
	}
	if err := validatePercent(targetExposure); err != nil {
		return PrepPlan{}, contracts.Invalid("invalid_target_exposure", "目标暴露比例必须位于 0 到 100 之间")
	}
	if targetExposure == 0 {
		return PrepPlan{}, contracts.Invalid("invalid_target_exposure", "目标暴露比例必须大于零")
	}
	return PrepPlan{
		ID:             id,
		SpecimenID:     specimen.ID,
		Objective:      goal,
		Method:         approach,
		Adhesive:       adhesiveValue,
		TargetExposure: targetExposure,
		Status:         PlanDraft,
		Version:        1,
		CreatedAt:      now.UTC(),
		UpdatedAt:      now.UTC(),
	}, nil
}

func (p PrepPlan) IsOpen() bool {
	return p.Status == PlanDraft ||
		p.Status == PlanApproved ||
		p.Status == PlanActive ||
		p.Status == PlanPaused
}

func (p PrepPlan) IsCompleted() bool {
	return p.Status == PlanCompleted
}

// CurrentPause 返回正在进行中的暂停记录；方案未暂停时第二返回值为 false。
func (p PrepPlan) CurrentPause() (PlanPause, bool) {
	if p.Status != PlanPaused || len(p.Pauses) == 0 {
		return PlanPause{}, false
	}
	last := p.Pauses[len(p.Pauses)-1]
	if last.ResumedAt != nil {
		return PlanPause{}, false
	}
	return last, true
}

// IsPaused 标识方案当前是否处于暂停态，暂停期间禁止追加任何进度。
func (p PrepPlan) IsPaused() bool {
	_, ok := p.CurrentPause()
	return ok
}

func (p *PrepPlan) Approve(approvedBy string, now time.Time) error {
	if p.Status != PlanDraft {
		return contracts.Conflict("plan_not_approvable", "只有草稿方案可以审批")
	}
	approver, err := requiredText("审批人", approvedBy, 80)
	if err != nil {
		return err
	}
	stamp := now.UTC()
	p.Status = PlanApproved
	p.ApprovedBy = approver
	p.ApprovedAt = &stamp
	p.Version++
	p.UpdatedAt = stamp
	return nil
}

func (p *PrepPlan) Begin(now time.Time) error {
	if p.Status == PlanActive {
		return nil
	}
	if p.Status != PlanApproved {
		return contracts.Conflict("plan_not_active", "只有已审批方案可以开始制备")
	}
	p.Status = PlanActive
	p.Version++
	p.UpdatedAt = now.UTC()
	return nil
}

// Pause 只允许活动方案带原因和责任人进入暂停；重复暂停及在非法状态上
// 暂停都会被拒绝，保证非法状态不会产生半截暂停记录。
func (p *PrepPlan) Pause(reason string, pausedBy string, now time.Time) error {
	if p.Status == PlanPaused {
		return contracts.Conflict("plan_already_paused", "方案已暂停，不能重复暂停")
	}
	if p.Status != PlanActive {
		return contracts.Conflict("plan_not_pausable", "只有进行中的活动方案可以暂停")
	}
	reasonValue, err := requiredText("暂停原因", reason, 200)
	if err != nil {
		return err
	}
	owner, err := requiredText("暂停责任人", pausedBy, 80)
	if err != nil {
		return err
	}
	stamp := now.UTC()
	p.Pauses = append(p.Pauses, PlanPause{
		Reason:   reasonValue,
		PausedBy: owner,
		PausedAt: stamp,
	})
	p.Status = PlanPaused
	p.Version++
	p.UpdatedAt = stamp
	return nil
}

// Resume 只允许暂停中的方案由当前负责人恢复；重复恢复、缺少责任人都
// 会被拒绝。恢复时闭合最后一条暂停记录，保留完整暂停历史。
func (p *PrepPlan) Resume(resumedBy string, now time.Time) error {
	if p.Status != PlanPaused {
		return contracts.Conflict("plan_not_paused", "只有暂停中的方案可以恢复")
	}
	if len(p.Pauses) == 0 || p.Pauses[len(p.Pauses)-1].ResumedAt != nil {
		return contracts.Conflict("plan_pause_missing", "方案没有进行中的暂停记录")
	}
	owner, err := requiredText("恢复责任人", resumedBy, 80)
	if err != nil {
		return err
	}
	stamp := now.UTC()
	p.Pauses[len(p.Pauses)-1].ResumedBy = owner
	p.Pauses[len(p.Pauses)-1].ResumedAt = &stamp
	p.Status = PlanActive
	p.Version++
	p.UpdatedAt = stamp
	return nil
}

func (p *PrepPlan) Complete(now time.Time) error {
	if p.Status == PlanCompleted {
		return nil
	}
	if p.Status != PlanActive {
		return contracts.Conflict("plan_not_completable", "只有进行中的方案可以完成")
	}
	p.Status = PlanCompleted
	p.Version++
	p.UpdatedAt = now.UTC()
	return nil
}

func (p *PrepPlan) Cancel(now time.Time) error {
	if p.Status != PlanDraft {
		return contracts.Conflict("plan_not_cancellable", "只有草稿方案可以取消")
	}
	p.Status = PlanCancelled
	p.Version++
	p.UpdatedAt = now.UTC()
	return nil
}
