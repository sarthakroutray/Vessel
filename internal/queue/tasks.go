package queue

import (
	"encoding/json"

	"github.com/hibiken/asynq"
)

const TypeEmailDeliver = "email:deliver"

// EmailTaskPayload carries only the log ID so the worker can fetch full
// details from the database (user, route, encrypted credentials, etc.).
type EmailTaskPayload struct {
	LogID string `json:"log_id"`
}

// NewEmailDeliveryTask serialises the payload and returns an asynq task.
func NewEmailDeliveryTask(logID string) (*asynq.Task, error) {
	payload, err := json.Marshal(EmailTaskPayload{LogID: logID})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TypeEmailDeliver, payload), nil
}
