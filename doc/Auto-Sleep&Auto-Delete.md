## 设计目标

* 针对一段时间内不活跃的空间，将其内的Deployment、DeamonSet、Statefulset 和 RelicaSet的副本数设为0，以达到回收物理资源的目的
* 对于处于Sleep模式的空间，当检测到活跃行为时，能够自动恢复其副本数
* 当检测到处于Sleep模式的空间超过一定时间之后，系统会自动删除该空间，以达到彻底回收资源的目标


## 如何定义活跃？

* "有效自然人"访问Namespace下资源的行为，包括CREATE/DELETE/UPDATE/LIST 都可以定义为活跃

## 设计要点

空间NS对象上引入特定Annotation，用于记录活跃信息

```
kubefree.io/status:'{"LastActivityTime":"2021-05-24T12:47:59Z","ActionBy":"ListPods", "UserInfo":{"name":"abc", "uid":"xxxx"}}'
```

默认2周不活跃，空间将自动进入Sleep模式
```
kubefree.io/sleep: enabled
kubefree.io/status:'{"LastActivityTime":"2021-05-24T12:47:59Z","ActionBy":"ListPods", "UserInfo":{"name":"abc", "uid":"xxxx"}, "sleepmode":{"SleepFromTime":"2021-05-24T12:47:59Z"}}'
```

## 整体架构

* Custom Authorization Webhook 检测活跃与否
* 如检测到活跃事件，生成或更新 NamespaceActivity 对象
* Operator 轮询 NamespaceActivity对象
    * 若到达不活跃阈值，则将NamespaceActivity对应的空间置为不活跃
    * 若重新活跃，则将对应空间恢复
    * 若不活跃周期到达阈值，则删除对应空间

## 主要逻辑

* 通过动态准入控制Webhok来统计空间活跃度，并更新上述Annotation
* 通过Controller轮询NS，检查LastActivityTime时间
* 如果超过一定时间，空间一直不活跃，那么该空间将被置为Sleep模式，其下的资源副本数置会被置为0(会在Annotation上备份原始副本数)
    ```
    sleepmode.kubefree.io/legacy-replicas: 2
    ``` 

* 如果空间在Inactive也超过一定时间，则直接删除该空间
* 如果某空间处于Sleep状态，但其LastActivityTime被更新了，那么将自动取消Sleep模式，恢复该空间下的资源

## 其他需求
* 设置Sleep模式&Delete，最好要有告警

## 未明确的点
* 自定义Authorization服务，在部署上会不会太麻烦？

## FAQ

* 如果就是需要在集群中部署长时间运行的程序，该怎么办？
    * A: 这种需要特殊申请备案，之后程序会忽略此种情况(或者提供个特定annotation?)
* 如果没有使用kubectl操作空间，但是空间资源一直以服务端程序模式对外提供服务，比如http请求，这种情况怎么办？
    * 同上
    * 既然是开发测试环境，理论上在合适的时间内，服务一定会被更新
    * 如果服务期望长期存在，或者放宽这个合适的时间，可以通过配置(annotation)来实现
* 为什么不通过CRD模式实现？


参考资料
