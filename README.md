# Local Directory 数据源插件

Local Directory 是 WeKnora 的外部数据源插件，用于递归同步管理员授权的本地目录。

支持：

* 首次全量同步；
* 文件新增、修改和删除；
* 后续增量同步；
* 应用重启后继续使用已有 Cursor；
* 只读目录访问；
* 禁止网络访问。

## 环境要求

* Linux/amd64；
* Linux Kernel 5.6 及以上；
* Go 1.26 及以上；
* 支持外部插件的 WeKnora V1；
* 本机 Docker、cgroup v2、bpffs 和 BTF。

当前 SDK 尚未独立发布，因此构建时需要提供 WeKnora 源码目录。

## 构建与测试

假设 WeKnora 位于：

```text
/absolute/path/to/WeKnora
```

运行测试：

```bash
bash scripts/with-sdk.sh /absolute/path/to/WeKnora go test ./...
bash scripts/with-sdk.sh /absolute/path/to/WeKnora go test -race ./...
```

构建插件：

```bash
bash scripts/build.sh /absolute/path/to/WeKnora
```

默认输出：

```text
dist/community.local-directory/
├── plugin.yaml
└── bin/
    └── plugin-linux-amd64
```

输出目录必须不存在。需要指定其他目录时：

```bash
bash scripts/build.sh \
  /absolute/path/to/WeKnora \
  /absolute/new/output/directory
```

## 安装

将完整产物目录放入 WeKnora 配置的插件目录，例如：

```text
/srv/wkp/packages/community.local-directory/
├── plugin.yaml
└── bin/
    └── plugin-linux-amd64
```

然后重启 WeKnora。V1 只在应用启动时扫描插件目录，不支持运行期热安装。

启动后：

1. 在 WeKnora 中找到 `community.local-directory`；
2. 创建对应的 DataSource；
3. 为 DataSource 授权一个 allow-root 下的目录；
4. 启用插件实例；
5. 执行资源查询或同步。

插件配置使用：

```json
{
  "type": "local_directory",
  "credentials": {},
  "resource_ids": ["grant_root"],
  "settings": {}
}
```

插件不接受 Host 路径、网络地址、凭据或其他自定义设置。实际目录必须通过 WeKnora 的 Directory Grant 授权。

## 同步行为

* 首次同步读取授权目录中的全部有效文件；
* 无变化时不会重新处理文件；
* 修改一个文件时，只返回该文件的新版本；
* 删除文件时返回对应删除事件；
* 文件改名按照“删除旧路径、添加新路径”处理；
* 同步失败时不会提交新的 Cursor；
* 文件解析、分块、Embedding 和索引由 WeKnora 完成。

## 使用限制

* 单个文件大小为 1 字节至 32 MiB；
* 单次最多同步 1024 个文件；
* 最多扫描 4096 个目录项；
* 最大目录深度为 32；
* 相对路径最长为 512 个 UTF-8 字节；
* 空文件不支持；
* 软链接、设备、FIFO、Socket 和非法文件名会被拒绝；
* 授权目录以只读方式挂载；
* 插件运行时不能访问网络；
* 当前仅支持单 Linux Plugin Controller 和本机 Docker。

## 验证

验证正式 Docker Runtime：

```bash
C2_ARTIFACT=/absolute/path/to/dist/community.local-directory \
  bash /absolute/path/to/WeKnora/scripts/test-plugin-backend.sh local-directory
```

验证完整同步、增量更新和应用重启恢复：

```bash
C2_ARTIFACT=/absolute/path/to/dist/community.local-directory \
  bash /absolute/path/to/WeKnora/scripts/test-plugin-application.sh
```
