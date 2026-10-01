package station

import (
	"context"

	"example.com/paleo-prep-station/internal/contracts"
	"example.com/paleo-prep-station/internal/domain"
	"example.com/paleo-prep-station/internal/repository"
)

func (s *Service) PausePlan(
	ctx context.Context,
	planID string,
	request contracts.PausePlanRequest,
) (domain.PrepPlan, error) {
	if err := ctx.Err(); err != nil {
		return domain.PrepPlan{}, err
	}

	var paused domain.PrepPlan
	err := s.repo.Mutate(func(m *repository.Mutation) error {
		plan, ok := m.GetPlan(planID)
		if !ok {
			return notFoundPlan(planID)
		}
		specimen, ok := m.GetSpecimen(plan.SpecimenID)
		if !ok {
			return notFoundSpecimen(plan.SpecimenID)
		}
		// 先做实体状态校验再修改任何字段；任一步失败整个变更副本被丢弃，
		// 不会留下方案已暂停而标本仍制备中的半截状态。
		if err := plan.Pause(request.Reason, request.PausedBy, s.now()); err != nil {
			return err
		}
		if err := specimen.MarkOnHold(s.now()); err != nil {
			return err
		}
		m.PutPlan(plan)
		m.PutSpecimen(specimen)
		paused = plan
		return nil
	})
	return paused, err
}

func (s *Service) ResumePlan(
	ctx context.Context,
	planID string,
	request contracts.ResumePlanRequest,
) (domain.PrepPlan, error) {
	if err := ctx.Err(); err != nil {
		return domain.PrepPlan{}, err
	}

	var resumed domain.PrepPlan
	err := s.repo.Mutate(func(m *repository.Mutation) error {
		plan, ok := m.GetPlan(planID)
		if !ok {
			return notFoundPlan(planID)
		}
		specimen, ok := m.GetSpecimen(plan.SpecimenID)
		if !ok {
			return notFoundSpecimen(plan.SpecimenID)
		}
		// 恢复同样成对提交：方案回到活动态与标本回到制备态必须同时生效。
		if err := plan.Resume(request.ResumedBy, s.now()); err != nil {
			return err
		}
		if err := specimen.MarkPreparing(s.now()); err != nil {
			return err
		}
		m.PutPlan(plan)
		m.PutSpecimen(specimen)
		resumed = plan
		return nil
	})
	return resumed, err
}
