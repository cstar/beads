package db

import (
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/types"
)

// Regression tests for the proxied-server create paths leaving the
// denormalized is_blocked flag stale: a bead created with an open blocker
// (via --deps, --parent on a blocked parent, or a graph plan) surfaced in
// bd ready and woke pool agents in a loop (gastownhall/gascity ga-hwe7j).

func (s *testSuite) TestBlockedStateOnCreatePaths() {
	s.Run("CreateWithBlocksDepMarksBlocked", s.createWithBlocksDepMarksBlocked)
	s.Run("CreateWithClosedBlockerStaysReady", s.createWithClosedBlockerStaysReady)
	s.Run("CreateSwapDirectionMarksExistingSource", s.createSwapDirectionMarksExistingSource)
	s.Run("ApplyIssueGraphMarksDependents", s.applyIssueGraphMarksDependents)
	s.Run("CreateWispWithBlocksDepMarksBlocked", s.createWispWithBlocksDepMarksBlocked)
	s.Run("AddDependencyUseCaseMarksSource", s.addDependencyUseCaseMarksSource)
}

func (s *testSuite) isBlockedOf(table, id string) int {
	var v int
	s.Require().NoError(s.Runner().QueryRowContext(s.Ctx(),
		"SELECT is_blocked FROM "+table+" WHERE id = ?", id).Scan(&v))
	return v
}

func (s *testSuite) mustCreate(uc domain.IssueUseCase, params domain.CreateIssueParams) *types.Issue {
	res, err := uc.CreateIssue(s.Ctx(), params, "tester")
	s.Require().NoError(err)
	return res.Issue
}

func (s *testSuite) createWithBlocksDepMarksBlocked() {
	s.resetMintConfig("bst", "")
	uc := s.issueUseCase()

	blocker := s.mustCreate(uc, domain.CreateIssueParams{
		Issue: &types.Issue{Title: "open blocker", IssueType: types.TypeTask, Priority: 2},
	})
	dependent := s.mustCreate(uc, domain.CreateIssueParams{
		Issue: &types.Issue{Title: "born blocked", IssueType: types.TypeTask, Priority: 2},
		Dependencies: []domain.DependencySpec{
			{Type: types.DepBlocks, TargetID: blocker.ID},
		},
	})

	s.Equal(0, s.isBlockedOf("issues", blocker.ID))
	s.Equal(1, s.isBlockedOf("issues", dependent.ID), "issue created with an open blocker must be born blocked")

	ready, err := uc.GetReadyWork(s.Ctx(), types.WorkFilter{})
	s.Require().NoError(err)
	for _, iss := range ready {
		s.NotEqual(dependent.ID, iss.ID, "blocked issue must not appear in ready work")
	}
}

func (s *testSuite) createWithClosedBlockerStaysReady() {
	s.resetMintConfig("bst", "")
	uc := s.issueUseCase()

	blocker := s.mustCreate(uc, domain.CreateIssueParams{
		Issue: &types.Issue{Title: "closed blocker", IssueType: types.TypeTask, Priority: 2, Status: types.StatusClosed},
	})
	dependent := s.mustCreate(uc, domain.CreateIssueParams{
		Issue: &types.Issue{Title: "satisfied dep", IssueType: types.TypeTask, Priority: 2},
		Dependencies: []domain.DependencySpec{
			{Type: types.DepBlocks, TargetID: blocker.ID},
		},
	})

	s.Equal(0, s.isBlockedOf("issues", dependent.ID), "a closed blocker must not block the new issue")
}

func (s *testSuite) createSwapDirectionMarksExistingSource() {
	s.resetMintConfig("bst", "")
	uc := s.issueUseCase()

	existing := s.mustCreate(uc, domain.CreateIssueParams{
		Issue: &types.Issue{Title: "pre-existing dependent", IssueType: types.TypeTask, Priority: 2},
	})
	s.Equal(0, s.isBlockedOf("issues", existing.ID))

	// "the new issue blocks existing" — swap stores existing -> new.
	s.mustCreate(uc, domain.CreateIssueParams{
		Issue: &types.Issue{Title: "new blocker", IssueType: types.TypeTask, Priority: 2},
		Dependencies: []domain.DependencySpec{
			{Type: types.DepBlocks, TargetID: existing.ID, SwapDirection: true},
		},
	})

	s.Equal(1, s.isBlockedOf("issues", existing.ID), "swap-direction dep must mark the pre-existing source blocked")
}

func (s *testSuite) applyIssueGraphMarksDependents() {
	s.resetMintConfig("bst", "")
	uc := s.issueUseCase()

	res, err := uc.ApplyIssueGraph(s.Ctx(), domain.GraphPlan{
		Nodes: []domain.GraphNode{
			{Key: "root", Issue: &types.Issue{Title: "graph root", IssueType: types.TypeTask, Priority: 2}},
			{Key: "step", Issue: &types.Issue{Title: "graph step", IssueType: types.TypeTask, Priority: 2}},
		},
		Edges: []domain.GraphEdge{
			{FromKey: "step", ToKey: "root", Type: types.DepBlocks},
		},
	}, "tester")
	s.Require().NoError(err)

	rootID := res.IDs["root"]
	stepID := res.IDs["step"]
	s.Require().NotEmpty(rootID)
	s.Require().NotEmpty(stepID)

	s.Equal(0, s.isBlockedOf("issues", rootID))
	s.Equal(1, s.isBlockedOf("issues", stepID), "graph node with an open blocks edge must be born blocked")

	ready, err := uc.GetReadyWork(s.Ctx(), types.WorkFilter{})
	s.Require().NoError(err)
	for _, iss := range ready {
		s.NotEqual(stepID, iss.ID, "blocked graph node must not appear in ready work")
	}
}

func (s *testSuite) createWispWithBlocksDepMarksBlocked() {
	s.resetMintConfig("bst", "")
	uc := s.issueUseCase()

	blocker, err := uc.CreateWisp(s.Ctx(), domain.CreateIssueParams{
		Issue: &types.Issue{Title: "open wisp blocker", IssueType: types.TypeTask, Priority: 2, Ephemeral: true},
	}, "tester")
	s.Require().NoError(err)

	dependent, err := uc.CreateWisp(s.Ctx(), domain.CreateIssueParams{
		Issue: &types.Issue{Title: "born blocked wisp", IssueType: types.TypeTask, Priority: 2, Ephemeral: true},
		Dependencies: []domain.DependencySpec{
			{Type: types.DepBlocks, TargetID: blocker.Issue.ID},
		},
	}, "tester")
	s.Require().NoError(err)

	s.Equal(0, s.isBlockedOf("wisps", blocker.Issue.ID))
	s.Equal(1, s.isBlockedOf("wisps", dependent.Issue.ID), "wisp created with an open blocker must be born blocked")
}

func (s *testSuite) addDependencyUseCaseMarksSource() {
	s.resetMintConfig("bst", "")
	uc := s.issueUseCase()
	depUC := domain.NewDependencyUseCase(NewDependencySQLRepository(s.Runner()))

	blocker := s.mustCreate(uc, domain.CreateIssueParams{
		Issue: &types.Issue{Title: "blocker for add", IssueType: types.TypeTask, Priority: 2},
	})
	source := s.mustCreate(uc, domain.CreateIssueParams{
		Issue: &types.Issue{Title: "source for add", IssueType: types.TypeTask, Priority: 2},
	})
	s.Equal(0, s.isBlockedOf("issues", source.ID))

	s.Require().NoError(depUC.AddDependency(s.Ctx(), &types.Dependency{
		IssueID:     source.ID,
		DependsOnID: blocker.ID,
		Type:        types.DepBlocks,
	}, "tester"))

	s.Equal(1, s.isBlockedOf("issues", source.ID), "domain AddDependency must mark the source blocked")
}
