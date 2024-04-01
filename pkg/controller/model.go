package controller

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	SleepAfterLabel     = "sleepmode.kubefree.com/sleep-after"
	DeleteAfterLabel    = "sleepmode.kubefree.com/delete-after"
	ExecutionStateLabel = "sleepmode.kubefree.com/state"

	NamespaceActivityStatusAnnotation = "sleepmode.kubefree.com/activity-status"
	LegacyReplicasAnnotation          = "sleepmode.kubefree.com/legacy-replicas"
)

// execution state of namespace controlled by kubefree
const (
	//
	NORMAL   string = "normal"
	SLEEPING string = "sleeping"
	SLEEP    string = "sleep"
	DELETING string = "deleting"
)

type activity struct {
	LastActivityTime customTime `json:"LastActivityTime"`
	Action           string     `json:"Action"`
	Resource         string     `json:"Resource"`
	Namespace        string     `json:"Namespace"`
	User             userInfo   `json:"UserInfo"`
}

func (a activity) String() string {
	return fmt.Sprintf("LastActivityTime: %v, Action: %v, Resource: %v, Namespace: %v, UserInfo: %v", a.LastActivityTime, a.Action, a.Resource, a.Namespace, a.User)
}

func getActivity(src string) (*activity, error) {
	activity := &activity{}
	err := json.Unmarshal([]byte(src), activity)
	if err != nil {
		return nil, err
	}
	return activity, nil
}

// userInfo is the user information of the last activity
// this information is related to Rancher
type userInfo struct {
	Name        string `json:"Name"`
	Impersonate string `json:"Impersonate"`
	RancherName string `json:"RancherName,omitempty"`
}

type customTime time.Time

func (ct *customTime) UnmarshalJSON(b []byte) error {
	t, err := time.Parse(`"2006-01-02T15:04:05Z07:00"`, string(b))
	if err == nil {
		*ct = customTime(t)
		return nil
	}

	t, err = time.Parse(`"2006-01-02T15:04:05"`, string(b))
	if err != nil {
		return err
	}

	*ct = customTime(t.UTC())
	return nil
}

func (ct customTime) String() string {
	return time.Time(ct).String()
}

func (ct *customTime) Time() time.Time {
	return time.Time(*ct)
}
