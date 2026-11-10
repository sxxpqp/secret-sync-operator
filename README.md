# Secret Sync Operator

用 Go + controller-runtime 实现的 Kubernetes Secret 跨命名空间同步 Operator。独立项目，与 file-s3-backup 无关。

## 同步规则

在源 Secret 所在命名空间创建一个 SecretSync，列出目标命名空间。目标 Secret 与源同名，只同步 data 和 type，支持 Opaque、TLS、dockerconfigjson 等类型。

- 监听源 Secret 更新，自动同步所有目标。
- 监听目标更新或删除，自动修复被修改的内容、补回误删副本；每 30 秒重新核对一次。
- 目标 namespace 必须已存在，Operator 不创建 namespace。
- 不覆盖已存在的非托管同名 Secret，也不接管其他规则或旧规则 UID 创建的副本。出现冲突时 Ready=False；其他目标仍会继续同步。
- 源 Secret 删除、规则删除、移除某个目标 namespace、修改源 Secret 名称时，原目标副本保留，停止继续更新。没有自动删除逻辑，RBAC 也没有 Secret delete 权限。
- 不复制源的 ownerReferences、immutable、标签或注解。目标额外元数据保持不变。没有跨 namespace ownerReference，避免垃圾回收误删副本。
- 目标 immutable 且内容需要更新时，报告失败；不会先删后建。
- 拒绝将托管目标作为源，避免同步环；不允许把源 namespace 作为目标；拒绝同步 ServiceAccount token Secret。
- 默认不输出 Secret 内容或底层 Secret 写入错误正文。Ready 状态是本轮同步结果，持续失败会重试。

## 权限边界

部署模板使用 ClusterRole，能读取/监听集群所有 Secret，并在集群内创建和更新 Secret。只有受信任的管理员应获准创建、修改 SecretSync；目标托管注解不是对恶意集群写入者的隔离机制。源被限制在规则所在 namespace，不能直接指定其他 namespace 的源。

如果只需要少数 namespace，可将 Secret 访问权限拆成指定 namespace 的 Role/RoleBinding，但还需要调整 controller-runtime 的多 namespace cache 配置，不能只收紧 RBAC 后仍监听全局。本版本提供的是集群级部署。

## 构建和验证

需要 Go 1.26 和 Docker。依赖固定 controller-runtime v0.24.1 / Kubernetes Go API v0.36.0；这不是在目标集群验证通过的版本声明。实际安装前应在使用的 Kubernetes 版本执行 server-side dry-run 和测试。

```bash
go mod download
go test ./...
go vet ./...
go build -o bin/manager ./cmd

# 或者用 kubebuilder 标准 Makefile 目标：
#   make manifests   # 重新生成 CRD 和 RBAC（需要 controller-gen）
#   make generate    # 重新生成 deepcopy 代码
#   make build       # 编译 manager

# 替换为你自己的镜像仓库，先登录再推送
docker build -t YOUR-REGISTRY/secret-sync-operator:0.1.0 .
docker push YOUR-REGISTRY/secret-sync-operator:0.1.0
```

回归测试使用 controller-runtime fake client，覆盖创建、更新、漂移修复、误删补回、同名冲突、UID 冲突、不可变目标、源/规则删除保留、目标移除、非法源和事件映射。fake client 不替代真实 API Server 的 CRD、RBAC、watch 和准入验证。

## 安装

镜像地址在 config/default/kustomization.yaml 的 images 字段里配置（默认 ghcr.io/sxxpqp/secret-sync-operator:0.1.0），也可以用 `make deploy IMG=<完整地址>` 覆盖。私有镜像需要在 secret-sync-system 配置拉取凭据并添加 imagePullSecrets。默认只有一个副本，开启 leader election；Lease 和事件权限限定在 secret-sync-system。容器非 root、只读根文件系统，健康检查端口为 8081，指标端点关闭。

```bash
kubectl apply --dry-run=server -f config/crd/bases/sync.secrets.io_secretsyncs.yaml
kubectl apply -f config/crd/bases/sync.secrets.io_secretsyncs.yaml
kubectl wait --for=condition=Established crd/secretsyncs.sync.secrets.io --timeout=60s
kubectl apply -k config/default
kubectl -n secret-sync-system rollout status deployment/secret-sync-operator
```

确认源 Secret 已存在于 default、目标 mall-prod / mall-test 已存在，然后修改并应用 config/samples/sync_v1alpha1_secretsync.yaml：

```yaml
apiVersion: sync.secrets.io/v1alpha1
kind: SecretSync
metadata:
  name: registry-credentials
  namespace: default
spec:
  sourceSecretName: aliyun-registry
  targetNamespaces:
    - mall-prod
    - mall-test
```

```bash
kubectl apply --dry-run=server -f config/samples/sync_v1alpha1_secretsync.yaml
kubectl apply -f config/samples/sync_v1alpha1_secretsync.yaml
kubectl -n default get secretsync registry-credentials
kubectl -n default get secretsync registry-credentials -o jsonpath='{.status}'
kubectl -n mall-prod get secret aliyun-registry
kubectl -n mall-test get secret aliyun-registry
kubectl -n secret-sync-system logs deployment/secret-sync-operator --tail=100
```

发生 TargetSyncFailed 时检查目标 namespace 是否存在、Secret 的归属注解、immutable 和 RBAC。不要为了消除冲突直接删除业务正在使用的 Secret；先确认归属和内容，再手动处理。源规则删除重建会生成新 UID，保留的旧副本不会自动接管。

本地调试（需有测试集群 kubeconfig，CRD 已安装）：

```bash
go run ./cmd --leader-elect=false
```

## 更新与卸载

更新源 Secret 后，无需重新 apply 规则。挂载/使用 Secret 的应用是否热更新由应用和 Kubernetes 使用方式决定；Operator 不自动重启 Pod。通过环境变量注入的 Secret 通常需要重建 Pod 才能读取新值。

删除 SecretSync 或卸载 Operator 都保留目标 Secret。删除 CRD 会删除全部 SecretSync 配置，目标副本仍保留：

```bash
kubectl delete -k config/default
# 确认不再需要任何同步规则时再执行
kubectl delete -f config/crd/bases/sync.secrets.io_secretsyncs.yaml
```

卸载清单包括 secret-sync-system namespace，必须确认其中没有放置其他业务资源。目标 Secret 的清理是独立人工操作。

参考：[controller-runtime](https://github.com/kubernetes-sigs/controller-runtime)、[Kubernetes Secret](https://kubernetes.io/docs/concepts/configuration/secret/)。