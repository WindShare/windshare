# 创建 CLOUDFLARE_API_TOKEN：浏览器操作指南

这个 Token（令牌）是一串授权码，让 GitHub Actions 能把 WindShare 中转部署到你的 Cloudflare 账户。全程只需浏览器，不用安装软件或运行命令。

准备好 Cloudflare 账号，以及你自己的 WindShare GitHub 仓库（通常是 Fork 后的仓库）。使用自定义域名时，该域名需要已接入同一个 Cloudflare 账户，并显示为 **有效（Active）**。

## 1. 打开创建页面

1. 登录 [Cloudflare 的用户 API 令牌页面](https://dash.cloudflare.com/profile/api-tokens)。也可以从右上角头像进入 **我的个人资料（My Profile）→ API 令牌（API Tokens）**。
2. 点击 **创建令牌（Create Token）**。
3. 在 **自定义令牌（Custom token）** 区域点击 **开始使用（Get started）**。
4. **令牌名称（Token name）** 填 `WindShare GitHub Actions`，方便以后认出它。

这里填写的是备注名称；后面在 GitHub 填的 Secret 名称才必须是 `CLOUDFLARE_API_TOKEN`。页面流程可对照 [Cloudflare 创建指南](https://developers.cloudflare.com/fundamentals/api/get-started/create-token/)。

## 2. 选择权限

在 **权限（Permissions）** 区域，每行从左到右依次选择三个下拉框：

| 什么时候需要 | 资源 | 权限 | 权限级别 |
|---|---|---|---|
| 所有人都需要 | 帐户（Account） | Workers | 管理员（Admin） |
| 使用自己的域名时加上 | 区域（Zone） | Workers 路由（Workers Routes） | 编辑（Edit / Write） |
| 使用自己的域名时加上 | 区域（Zone） | 区域（Zone） | 读取（Read） |

- 使用 Cloudflare 提供的 `xxx.workers.dev` 地址：只填第一行。
- 使用自己的地址，例如 `relay.example.com`：点击 **添加更多（Add more）**，补齐后两行，共三行。

首次部署需要创建 Worker，因此第一行选 **Workers → 管理员**；`Workers 脚本（Workers Scripts）→ 编辑` 不等同于这里的创建权限。这个管理员权限覆盖所选账户中的所有 Workers。自定义域名还需要对应区域的路由写入权限，见 [Cloudflare 权限说明](https://developers.cloudflare.com/workers/authorization/workers/)。区域读取权限用于本项目部署前检查域名归属。

## 3. 选择账户和域名范围

在 **帐户资源（Account Resources）** 中：

1. 左侧保留 **包括（Include）**。
2. 将 **所有帐户（All accounts）** 改为你准备部署中转的那个账户。

如果使用自定义域名，还会有 **区域资源（Zone Resources）**：

1. 选择 **包括（Include）→ 特定区域（Specific zone）**。
2. 在出现的下拉框中选择域名，例如 `example.com`。

这里选 Cloudflare 中托管的区域名。对于 `relay.example.com`，通常选的是 **`example.com`**，不是填写完整的中转地址。没有“区域资源”这一栏时，先检查上一步是否添加了两条区域权限。

其余选项：

- **客户端 IP 地址过滤（Client IP Address Filtering）**：留空。GitHub Actions 在云端运行，填写你电脑的 IP 会挡住它。
- **TTL（有效期）**：首次使用可保留默认值；如果设置到期时间，到期前需要更换 GitHub 中的 Token，才能继续自动部署。

## 4. 创建并复制 Token

1. 点击 **继续以显示摘要（Continue to summary）**。
2. 核对账户和权限。使用自定义域名时，摘要应类似下面的示意：

   ```text
   你的 Cloudflare 账户 — Workers：管理员
     └─ example.com — Workers 路由：编辑，区域：读取
   ```

3. 点击 **创建令牌（Create Token）**。
4. 在成功页面复制刚生成的那一长串 Token，先保留这个页面，接着做下一步。

**Token 只显示这一次。** 复制的是 Token 本身，不是下面的整段 `curl` 测试命令，也不要加 `Bearer` 或引号。把它直接存入下一步的 GitHub Secret，不要写进代码、提交记录或 Issue。

## 5. 存入自己的 GitHub 仓库

1. 新开一个标签页，进入**你自己的 WindShare 仓库**。Fork 用户应看到页面左上角是自己的用户名或组织名。
2. 点击仓库上方的 **Settings（设置）**。
3. 左侧进入 **Secrets and variables → Actions**。
4. 选择 **Secrets** 标签，点击 **New repository secret（新建仓库密钥）**。
5. 按下表填写：

   | 输入框 | 填什么 |
   |---|---|
   | **Name** | `CLOUDFLARE_API_TOKEN`，完整复制这个名称 |
   | **Secret** | 刚才从 Cloudflare 复制的那一长串 Token |

6. 点击 **Add secret（添加密钥）**。

保存后，**Repository secrets** 列表中应出现 `CLOUDFLARE_API_TOKEN`；看不到它的具体内容是正常的。注意存到 **Secrets**，不要存到旁边的 **Variables**。页面位置可对照 [GitHub 操作说明](https://docs.github.com/en/actions/how-tos/write-workflows/choose-what-workflows-do/use-secrets#creating-secrets-for-a-repository)。

## 6. 继续部署

Token 配置完成。回到[中转部署指南](cloudflare-relay.md#first-deployment)，继续生成本地 age 密钥并填写其余 Variables，然后运行 **Deploy private Cloudflare relay**。

保存 Token 本身不会启动部署。其余设置齐全后，在仓库的 **Actions → Deploy private Cloudflare relay → Run workflow** 中选择 `main`，再点击 **Run workflow**。运行成功后会出现绿色勾号。

## 遇到问题时

| 现象 | 怎么处理 |
|---|---|
| GitHub 找不到 Settings 或无法新建 Secret | 确认进入的是自己的仓库，并使用有仓库管理权限的 GitHub 账号。 |
| 部署提示权限不足或 HTTP 401 / 403 | 检查 Token 是否复制完整、是否过期，以及权限和账户范围是否按上文设置；自定义域名还要检查区域范围。 |
| 关掉页面后找不到 Token | 重新创建一个；在 GitHub 的同名 Secret 旁点击编辑按钮，粘贴新值并保存。 |

Cloudflare Token 用于部署，和 WindShare 分享时使用的注册凭据是两回事。更换这个 GitHub Secret 不会更换中转的注册凭据，也不需要重新导入客户端配置；日常推送更新不需要重新创建 Token。
