package ai

// Role只能是“system”|“user”|“assitant”
type Message struct {
	Role    string
	Content string
}
