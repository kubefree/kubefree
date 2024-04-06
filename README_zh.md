# kubefree - Automatically sleep or delete resources in Kubernetes for reclamation.

在日常研发迭代中，有些资源或环境并不需要长期存在，比如PR Preview 环境, 比如临时起的Pod等。这些资源如果长期不处理，会占用整体集群的资源，造成浪费。而Kubefree可以优雅的自动回收这些环境或资源。

### 原理

Kubefree有两种策略，以Label形式来标记:

* Sleep模式

```
sleepmode.kubefree.com/sleep-after: <duration>
``` 

如果资源带有这个Label，那么意味着，在经过`<duration>`时间后，kubefree会操作资源进入Sleep状态.
Sleep状态是一种资源回收的中间状态，主要是回收容器资源, 本质是将deployment/statefulset等资源的`replicas`置为0.
而在Sleep状态，如果资源重新进入活跃状态(PS: lastActivittTime 被更新为教新值)，资源将会被重新恢复(PS: `replicas` 复原).
但如果资源一致不活跃，那么重新等待`<duration>`时间后，资源将被彻底删除。


* Delete模式
```
sleepmode.kubefree.com/delete-after: <duration>
```
如果资源带有这个Label，那么意味着预期，在经过`<duration>`时间后，这个资源将会被直接删除。


### Quick Start

建设你已有存在的集群，可以一键部署:
```
kubectl apply -f deploy.yaml
```


### 应用实例

[参考例子](https://github.com/goplus/builder/blob/dev/scripts/pr-preview.sh#L23)

### 关于 lastActivityTime 

* 这个时间代表着，目标资源上次的活跃时间，这个活跃时间的最终设别应该跟您的实际预期一致。
    * 比如访问这个服务，算不算活跃？比如get 查看下这个资源算不算活跃？ 这些问题看起来并没有
* 当前kubefree并没有提供自动更新lastActivityTime的官方姿势，

## RoadMap 

* 从安全角度考虑，比如引入白名单能力
* 基于常用场景自动更新活跃
* 支持更多资源类型的回收

### 如果你喜欢或者在使用kubefree，请star 我们









