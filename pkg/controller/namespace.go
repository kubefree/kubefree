package plank

import (
	"encoding/json"
	"time"
)

const (
	// TODO: 改成Duration
	SleepAfterSecondsLabel  = "sleepmode.kubefree.com/sleep-after-seconds"
	DeleteAfterSecondsLabel = "sleepmode.kubefree.com/delete-after-seconds"
	ExecutionStateLabel     = "sleepmode.kubefree.com/state"

	NamespaceActivityStatusAnnotation = "sleepmode.kubefree.com/activity-status"
	LegacyReplicasAnnotation          = "sleepmode.kubefree.com/legacy-replicas"
)

// execution state of namespace controlled by kubefree
const (
	NORMAL   string = "normal"
	SLEEPING string = "sleeping"
	SLEEP    string = "sleep"
	DELETING string = "deleting"
)

type userInfo struct {
	Name        string `json:"Name"`
	Impersonate string `json:"Impersonate"`
	RancherName string `json:"RancherName, omitempty"`
}
type activity struct {
	LastActivityTime time.Time `json:"LastActivityTime"`
	Action           string    `json:"Action"`
	Resource         string    `json:"Resource"`
	Namespace        string    `json:"Namespace"`
	User             userInfo  `json:"UserInfo"`
}

func getActivity(src string) (*activity, error) {
	activity := &activity{}
	err := json.Unmarshal([]byte(src), activity)
	if err != nil {
		return nil, err
	}
	return activity, nil
}
