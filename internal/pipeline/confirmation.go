package pipeline

// Confirmation decision vocabulary, shared by the LLM classifier
// (ClassifyConfirmation) and the orchestrator's confirmation-resolution
// blocks. These canonical strings also match the deterministic frontend
// button signal carried in inbound metadata["confirmation"].
//
//   - approve: the user consents to the pending action exactly as proposed.
//   - amend:   the user gives a correction/new instruction; the pending call
//     is dropped and the turn re-plans (raising a fresh confirmation).
//   - reject:  the user cancels, OR the reply can't be reliably interpreted
//     ("im Zweifel reject").
//
// There is deliberately no word-list / keyword matching: free-text replies are
// classified by the LLM (language-agnostic), and the button path is an explicit
// structured signal. Anything the classifier can't resolve falls to reject.
const (
	DecisionApprove = "approve"
	DecisionAmend   = "amend"
	DecisionReject  = "reject"
)
