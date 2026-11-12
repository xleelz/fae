package tracker

import (
	"context"
	"strings"
)

type linearPageInfo struct {
	HasNextPage *bool   `json:"hasNextPage"`
	EndCursor   *string `json:"endCursor"`
}

func connectionError(category, message string) error {
	return &Error{Category: category, Message: message}
}

// completeIssueConnections never filters incoming blockers by the candidate's
// project. Missing initial metadata is visible but never dispatchable; once a
// connection advertises more pages, any incomplete continuation fails the poll.
func (a *LinearAdapter) completeIssueConnections(ctx context.Context, node *linearIssueNode) error {
	for _, kind := range []string{"blockers", "labels", "children"} {
		var info *linearPageInfo
		var malformed bool
		var count int
		current := func() {
			switch kind {
			case "blockers":
				info = node.InverseRelations.PageInfo
				malformed = node.InverseRelations.Malformed
				count = len(node.InverseRelations.Nodes)
			case "labels":
				info = node.Labels.PageInfo
				malformed = node.Labels.Malformed
				count = len(node.Labels.Nodes)
			case "children":
				info = node.Children.PageInfo
				malformed = node.Children.Malformed
				count = len(node.Children.Nodes)
			}
		}
		current()
		if count > linearNestedNodeLimit {
			return connectionError(ErrCategoryPagination, "Linear nested connection exceeds node bound")
		}
		if malformed || info == nil || info.HasNextPage == nil {
			node.CollectionsIncomplete = true
			continue
		}
		seen := map[string]bool{}
		pages := 1
		for *info.HasNextPage {
			if pages >= linearNestedPageLimit || count >= linearNestedNodeLimit {
				return connectionError(ErrCategoryPagination, "Linear nested connection exceeds pagination bound")
			}
			cursor := ""
			if info.EndCursor != nil {
				cursor = strings.TrimSpace(*info.EndCursor)
			}
			if cursor == "" || seen[cursor] {
				return connectionError(ErrCategoryPagination, "Linear nested connection has missing or repeated endCursor")
			}
			seen[cursor] = true
			query := linearBlockersPageQuery
			if kind == "labels" {
				query = linearLabelsPageQuery
			} else if kind == "children" {
				query = linearChildrenPageQuery
			}
			var data struct {
				Issue *struct {
					ID               string                         `json:"id"`
					InverseRelations linearRelationConnection       `json:"inverseRelations"`
					Labels           linearLabelConnection          `json:"labels"`
					Children         linearHierarchyIssueConnection `json:"children"`
				} `json:"issue"`
			}
			if err := a.doGraphQL(ctx, query, map[string]any{"id": node.ID, "first": linearPageSize, "after": cursor}, &data); err != nil {
				return err
			}
			if data.Issue == nil || data.Issue.ID != node.ID {
				return connectionError(ErrCategoryResponse, "Linear nested connection issue is missing or changed")
			}
			switch kind {
			case "blockers":
				next := data.Issue.InverseRelations
				node.InverseRelations.Nodes = append(node.InverseRelations.Nodes, next.Nodes...)
				node.InverseRelations.PageInfo = next.PageInfo
				node.InverseRelations.Malformed = next.Malformed
			case "labels":
				next := data.Issue.Labels
				node.Labels.Nodes = append(node.Labels.Nodes, next.Nodes...)
				node.Labels.PageInfo = next.PageInfo
				node.Labels.Malformed = next.Malformed
			case "children":
				next := data.Issue.Children
				node.Children.Nodes = append(node.Children.Nodes, next.Nodes...)
				node.Children.PageInfo = next.PageInfo
				node.Children.Malformed = next.Malformed
			}
			pages++
			current()
			if malformed || info == nil || info.HasNextPage == nil {
				return connectionError(ErrCategoryResponse, "Linear nested connection continuation is incomplete")
			}
			if count > linearNestedNodeLimit {
				return connectionError(ErrCategoryPagination, "Linear nested connection exceeds node bound")
			}
		}
	}
	return nil
}
