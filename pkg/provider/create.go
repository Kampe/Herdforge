package provider

import "fmt"

// ValidateCreateTask is the shared create-task field check used by memory
// and local providers.
func ValidateCreateTask(task *Task) error {
	if task == nil || task.Title == "" || task.ProjectID == "" {
		return fmt.Errorf("create task: title and project are required")
	}
	return nil
}
