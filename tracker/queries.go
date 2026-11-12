package tracker

const linearIssueFieldsFragment = `
      id
      identifier
      number
      title
      description
      priority
      branchName
      url
      createdAt
      updatedAt
      state {
        id
        name
        type
      }
      assignee {
        id
      }
      team {
        id
        key
      }
      project {
        id
        slugId
      }
      parent {
        id
        identifier
        title
        state {
          name
        }
      }
      children(first: 50) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          identifier
          title
          state {
            name
          }
        }
      }
      labels(first: 50) {
        pageInfo { hasNextPage endCursor }
        nodes {
          name
        }
      }
      inverseRelations(first: 50) {
        pageInfo { hasNextPage endCursor }
        nodes {
          type
          issue {
            id
            identifier
            state {
              name
              type
            }
          }
        }
      }
`

const fetchIssuesByStatesQuery = `query FetchIssuesByStates($first: Int!, $after: String, $filter: IssueFilter) {
  issues(first: $first, after: $after, filter: $filter) {
    pageInfo {
      hasNextPage
      endCursor
    }
    nodes {` + linearIssueFieldsFragment + `
    }
  }
}`

const fetchIssuesByIDsQuery = `query FetchIssuesByIDs($first: Int!, $after: String, $filter: IssueFilter) {
  issues(first: $first, after: $after, filter: $filter) {
    pageInfo {
      hasNextPage
      endCursor
    }
    nodes {` + linearIssueFieldsFragment + `
    }
  }
}`

const workflowStatesQuery = `query ResolveWorkflowStates($first: Int!, $after: String, $filter: WorkflowStateFilter) {
  workflowStates(first: $first, after: $after, filter: $filter) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id
      name
      type
      team {
        id
        key
      }
    }
  }
}`

const issueUpdateStateMutation = `mutation TransitionIssueState($id: String!, $stateId: String!) {
  issueUpdate(id: $id, input: { stateId: $stateId }) {
    success
    issue {` + linearIssueFieldsFragment + `
    }
  }
}`

const commentCreateMutation = `mutation CreateIssueComment($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) {
    success
    comment {
      id
      body
      url
    }
  }
}`

// The issue lookup is deliberately unfiltered by project: incoming blockers may
// belong to a different project or team and still constrain this issue.
const linearIssueConnectionsPrefix = `query FetchIssueConnection($id: String!, $first: Int!, $after: String) { issue(id: $id) { id `
const linearChildrenPageQuery = linearIssueConnectionsPrefix + `children(first:$first, after:$after) { pageInfo { hasNextPage endCursor } nodes { id identifier title state { name type } } } } }`
const linearLabelsPageQuery = linearIssueConnectionsPrefix + `labels(first:$first, after:$after) { pageInfo { hasNextPage endCursor } nodes { name } } } }`
const linearBlockersPageQuery = linearIssueConnectionsPrefix + `inverseRelations(first:$first, after:$after) { pageInfo { hasNextPage endCursor } nodes { type issue { id identifier state { name type } } } } }`

const issueForStateWriteQuery = `query GetLinearIssueForStateWrite($id: String!) { issue(id: $id) { id identifier title state { id name type } team { id key } project { id slugId } } }`
