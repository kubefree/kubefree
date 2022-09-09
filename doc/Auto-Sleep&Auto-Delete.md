
## 设计目标

* 针对一段时间内不活跃的空间，支持将其置为Sleep模式 (将其内的Deployment、DeamonSet、Statefulset 和 RelicaSet的副本数设为0，以达到回收物理资源的目的)
* 对于处于Sleep模式的空间，当检测到活跃行为时，能够自动恢复其副本数
    * 当检测到处于Sleep模式的空间超过一定时间之后，系统会自动删除该空间，以达到彻底回收资源的目标
    * 对于某些不活跃的空间，支持不经过Sleep模式，提供直接删除的能力
## 如何定义活跃？

"活跃" 当前直观理解为 "有人用"，所以对于namespace下资源的任何操作，包括CREATE/UPDATE/LIST/DELETE等都视为为活跃行为

## 设计要点

空间NS对象上引入特定Annotation，用于记录活跃信息

    // 标记namespace的活跃时间和事件
    sleepmode.kubefree.com/activity-status:'{"LastActivityTime":"2021-05-24T12:47:59Z","ActionBy":"ListPods", "UserInfo":{"name":"abc", "uid":"xxxx"}}'

空间NS对象上引入特定Label，用于标记希望被kubefree管理的空间

    // 用户设置，标记希望空间在多长时间不活跃之后置为sleep模式
    sleepmode.kubefree.com/sleep-after: 2h // time duration, If set to non zero, kubefree will sleep namespace after specified seconds of inactivity
    // 用户设置，标记希望空间在多长时间不活跃之后直接删除
    sleepmode.kubefree.com/delete-after: 2h // time duration, If set to non zero, kubefree will delete namespace after specified seconds of inactivity
    // kubefree 会自动设置，标记空间sleepmode下的状态
    // normal： 代表有从sleep/sleeping状态 恢复
    // sleeping: 正在将改空间下的副本数缩小为0
    // sleep: 已经将改空间下的副本数缩小为0
    // deleting: kubefree正在删除目标空间
    sleepmode.kubefree.com/state: normal/sleeping/sleep/deleting // indicates the detailed state of namespace in sleep mode
    
空间下的副本资源，比如deployment、statefulset、ReplicaSet 等引入Annotation

    // kubefree自动设置，用于记录原始副本数
    sleepmode.kubefree.com/legacy-replicas: 2

## 主要逻辑

* 通过activity-status 来标记namespace的活跃时间
* kubefree Controller通过infomer机制来检查
    * 先检测delete-after，再检测sleep-after
* sleep-after 和 delete-after是各自独立的逻辑，彼此不依赖，通常有以前使用场景
    * 只使用sleep-after，当空间处于sleep的情况下，再等sleep-after的时间，才会真正被删除
    * 只使用delete-after，到时就直接删除
    * 同时使用sleep-after和delete-after
        * 若delete-after > sleep-after, 那么空间将在sleep-after之后处于sleep，在delete-after之后被删除
        * 若delete-after <= sleep-after, 那么空间将在delete-after之后被删除，到达不了sleep状态

## 哪些空间会被标记和回收？


### 兜底方案

kubefree会维护一套系统空间白名单，而除此之外的空间都会打上以下标记：

    sleepmode.kubefree.com/sleep-after = 168h # 说明，空间一周内不活跃会被置为sleep。sleep状态又持续一周，会被彻底删除
### 主动方案

对于想缩短回收周期的空间，推荐直接打上删除或者Sleep标记。例子

    kubectl label --overwrite namespace xxx sleepmode.kubefree.com/sleep-after=24h # 一天内不活跃，空间会被置为sleep。如果sleep状态又持续一天，会被彻底删除
    kubectl label --overwrite namespace xxx sleepmode.kubefree.com/delete-after=24h # 一天内不活跃，会被直接删除


## TODO

* 支持自动检查和更新空间活跃度信息
