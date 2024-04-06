# kubefree - Automatically sleep or delete resources in Kubernetes for reclamation.

[中文](./README_zh.md)

In the daily development cycle, some resources or environments do not need to exist for an extended period, such as PR Preview environments or temporary Pods. If these resources are not handled for a long time, they can occupy cluster resources and cause waste. Kubefree gracefully automates the reclamation of these environments or resources.


### Principle

Kubefree has two strategies that are marked in the form of labels:

* Sleep mode

```
sleepmode.kubefree.com/sleep-after: <duration>
``` 

If a resource has this label, it means that after <duration> time has passed, kubefree will operate on the resource and put it into Sleep mode.
Sleep mode is an intermediate state of resource reclamation, primarily reclaiming container resources. Essentially, it sets the replicas of resources like deployments/statefulsets to 0.
While in Sleep mode, if the resource becomes active again (PS: lastActivityTime is updated with a newer value), the resource will be restored (PS: replicas restored).
However, if the resource remains inactive, it will be permanently deleted after waiting for <duration> time.


* Delete mode

```
sleepmode.kubefree.com/delete-after: <duration>
```

If a resource has this label, it means that after <duration> time has passed, the resource will be directly deleted.


### Quick Start

Assuming you already have an existing cluster, you can deploy it with a single command:

```
kubectl apply -f deploy.yaml
```


### Practice Example

[Practice Example](https://github.com/goplus/builder/pull/280#issuecomment-2033752952) 

### About lastActivityTime 

* This time represents the last active time of the target resource, and the determination of this active time should align with your actual expectations.
    * For example, does accessing this service count as activity? Does performing a get operation to view this resource count as activity? These questions may not have clear answers.
* Currently, Kubefree does not provide an official way to automatically update lastActivityTime.

## RoadMap 

* Consider security aspects, such as introducing whitelist capabilities.
* Automatically update activity based on common scenarios.
* Support reclamation of more types of resources.

### If you like or are using Kubefree, please star us.









