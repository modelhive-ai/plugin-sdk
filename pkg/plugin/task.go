package plugin

// The task judge: one call to a classifier model, made by the gateway when a
// prompt may start a new piece of work, that answers whether it does and every
// question the plugins in the request's pipeline ask about it (how hard it is,
// which context it needs...). Follow-ups, tool rounds and short replies belong
// to the task in progress and cost no call.
//
// A Processor asks by implementing TaskAsker; it reads the answers, and the
// task the request belongs to, in Request.Task. Questions asked by several
// plugins of one request go to the judge model in a single call.

// Kinds of TaskQuestion.
const (
	TaskChoice = "choice" // one of Options
	TaskYesNo  = "yes_no" // the probability of yes
)

// TaskQuestion is one question about the work a prompt asks for.
type TaskQuestion struct {
	// ID names the question within the plugin; answers are keyed by it.
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Question is put to the judge, such as "How hard is the work the new
	// message asks for?".
	Question string `json:"question"`
	// Options of a choice, by name, each with what it means: the judge reads
	// the meaning to choose. Unused for yes_no.
	Options map[string]string `json:"options,omitempty"`
}

// TaskQuestions is what a plugin asks for one request.
type TaskQuestions struct {
	// Model is the judge model: a SystemOne classifier (typed questions) or
	// any gateway chat model (asked for JSON). Plugins that name the same
	// model share one call.
	Model     string         `json:"model"`
	TimeoutMS int            `json:"timeout_ms,omitempty"`
	Questions []TaskQuestion `json:"questions"`
}

// TaskAsker is a Processor that asks the task judge questions. TaskQuestions
// is called for each request before any processor runs, with the request as
// the instance sees it (its group settings applied); nil or no questions asks
// nothing.
type TaskAsker interface {
	TaskQuestions(r *Request) *TaskQuestions
}

// Task is the piece of work a request belongs to.
type Task struct {
	// ID identifies the task; it stays the same for every request of it, on
	// every replica.
	ID string `json:"id"`
	// Session identifies the conversation, scoped to the caller.
	Session string `json:"session"`
	// Started is set on the request whose prompt began the task.
	Started bool `json:"started,omitempty"`
	// Judged is set when the judge was asked on this request: Answers then
	// holds its answers, for a new task or for a follow-up that may change
	// what the task needs.
	Judged bool `json:"judged,omitempty"`
	// Answers are this plugin's, by TaskQuestion.ID; nil when not Judged.
	Answers map[string]TaskAnswer `json:"answers,omitempty"`
	// Error is why the judge could not answer; the request goes on without.
	Error string `json:"error,omitempty"`
}

// TaskAnswer is the judge's answer to one question.
type TaskAnswer struct {
	// Choice is the option chosen, for a choice.
	Choice string `json:"choice,omitempty"`
	// Yes is the probability of yes, for a yes_no.
	Yes float64 `json:"yes,omitempty"`
}
