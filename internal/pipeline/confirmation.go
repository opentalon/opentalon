package pipeline

// Confirmation decision vocabulary, shared by the LLM classifier
// (ClassifyConfirmation) and the orchestrator's confirmation-resolution
// blocks. These canonical strings also match the deterministic frontend
// button signal carried in inbound metadata["confirmation"].
//
//   - approve: the user consents to the pending action exactly as proposed.
//   - amend:   the reply is neither a yes nor a no: a correction, another
//     instruction or a question. The pending call is dropped and the reply is
//     answered as an ordinary message (a write it leads to raises a fresh
//     confirmation).
//   - reject:  the user declines or cancels, OR the reply says nothing that
//     can be acted on or answered.
//
// Neither amend nor reject executes anything; only an unambiguous approve does.
// There is deliberately no word-list / keyword matching: free-text replies are
// classified by the LLM (language-agnostic), and the button path is an explicit
// structured signal. Anything the classifier can't resolve falls to reject.
const (
	DecisionApprove = "approve"
	DecisionAmend   = "amend"
	DecisionReject  = "reject"
)
