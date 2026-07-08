// Package link 实现能力链接(capability URL)的构造与解析(执行计划 §6.3)。
//
// 链接形如 https://<前端域>/<shareId>?r=<中转>#<base64url(suiteByte‖readSecret)>:
// fragment 是永不发往服务器的能力令牌;?r= 自始按多值列表解析(§6.3);
// 分离密钥(split-key)在此提供裸链接/密钥串的拆合(§6.10)。
// suite 常量以链接 fragment 首字节为语义家,定义于本包。纯函数、无 IO。
package link
