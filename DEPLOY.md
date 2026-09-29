# 部署说明

## 独立服务

- 域名：`saotu.osh.lol`
- 应用监听：`127.0.0.1:18887`
- 服务：`saotu-osh.service`
- 程序：`/opt/saotu-osh/app/saotu-osh-linux`
- 数据：`/var/lib/saotu-osh/saotu.sqlite3`
- 私有图片：`/var/lib/saotu-osh/media`
- 配置：`/etc/saotu-osh/app.env`
- 账号服务：`http://127.0.0.1:18766`

程序、数据库和媒体目录与提示词站、打卡站分开。发布时不得覆盖这两个站点的程序、数据库或配置。

## 发布前

1. 运行 README 中的测试和 Linux 编译命令。
2. 使用管理员自己的 SSH 登录方式上传发布目录。不要在命令行、脚本或仓库保存密码。
3. 核对已有 Nginx 的路径、站点配置、证书方式以及 18887 端口占用情况。
4. 从共享账号服务确认创始人的账号 ID。将它写入新站 `FOUNDER_USER_ID`，不是用户名，也不是密码。新用户注册不会自动成为创始人。
5. 复用服务器已有支付、邮件配置文件，通过 systemd 加载。新站必须覆盖支付通知 URL 为 `https://saotu.osh.lol/api/payments/notify/huifu`，避免回执到达别的站点。

## 安装

在上传目录执行：

```sh
FOUNDER_USER_ID=<已核实的数字ID> sh deploy/install.sh
```

安装脚本保存旧程序备份，并创建独立的数据目录。不自动修改 Nginx，也不替换已存在的 app.env。程序已经存在时，安装后再执行 `systemctl restart saotu-osh`。

将 `deploy/saotu.http.conf` 放到现有 Nginx 的站点目录，执行配置检查，通过后重载。Cloudflare 为 `saotu.osh.lol` 建立指向当前服务器的记录。申请该域名的证书后，使用 `deploy/saotu.tls.conf`，再次检查并重载 Nginx。不得跳过证书验证。

## 验收

```sh
systemctl is-active saotu-osh
curl -fsS http://127.0.0.1:18887/api/health
curl -fsS https://saotu.osh.lol/api/health
curl -fsS https://saotu.osh.lol/api/library/bootstrap
```

用浏览器检查桌面与手机布局、登录、投稿、审核、额度记录和会员入口。实际付款和邮箱投递需要独立验收；测试回执通过不代表真实资金链路已经验收。

首次启动会核对内置图片的授权清单、大小和 SHA-256，再导入 25 张已审核的首批图片。重复启动不会重复导入或发放投稿奖励。管理员可以继续投稿有分享权限的图片，再由其他用户审核。

## 备份与回滚

数据库使用 SQLite 在线备份或停服务后备份，并与 media 目录保持同一时间点。普通代码更新不上传数据库。

回滚时选用本次发布前保存的程序，替换后重启独立服务并检查健康接口。若修改过数据库或 Nginx，按各自备份单独恢复。
