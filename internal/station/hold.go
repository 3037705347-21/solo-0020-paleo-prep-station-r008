package station

import (
	"context"

	"example.com/paleo-prep-station/internal/contracts"
	"example.com/paleo-prep-station/internal/domain"
	"example.com/paleo-prep-station/internal/repository"
)

func (s *Service) HoldPlan(
	ctx context.Context,
	planID string,
	request contracts.HoldRequest,
) (domain.PrepPlan, error) {
	if err := ctx.Err(); err != nil {
		return domain.PrepPlan{}, err
	}

	var held domain.PrepPlan
	err := s.repo.Mutate(func(m *repository.Mutation) error {
		plan, ok := m.GetPlan(planID)
		if !ok {
			return notFoundPlan(planID)
		}
		specimen, ok := m.GetSpecimen(plan.SpecimenID)
		if !ok {
			return notFoundSpecimen(plan.SpecimenID)
		}
		if err := plan.Hold(request.Reason, request.Owner, s.now()); err != nil {
			return err
		}
		if err := specimen.MarkOnHold(s.now()); err != nil {
			return err
		}
		m.PutPlan(plan)
		m.PutSpecimen(specimen)
		held = plan
		return nil
	})
	return held, err
}

func (s *Service) ResumePlan(
	ctx context.Context,
	planID string,
	request contracts.ResumeRequest,
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
		if err := plan.Resume(request.ResumedBy, s.now()); err != nil {
			return err
		}
		if err := specimen.ResumePreparing(s.now()); err != nil {
			return err
		}
		m.PutPlan(plan)
		m.PutSpecimen(specimen)
		resumed = plan
		return nil
	})
	return resumed, err
}
