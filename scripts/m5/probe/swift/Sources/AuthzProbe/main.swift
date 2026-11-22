// M5-1 探查 E1、E2（m5-plan v2 第 5 节和表后“授权凭证的语义”）：系统授权服务的本人授权。
// 会弹系统授权框、会改授权数据库（只登记和删除我们自己的那一个权限项），所以只由用户经 scripts/m5/probe/run-e1e2.sh 执行；
// 开发会话只编译、不运行。
//
//   AuthzProbe register            （以 root）登记权限项 com.claude-abroad.sensitive：规则“本人认证、不共享、不缓存”
//   AuthzProbe show                显示这个权限项现在的规则（只读）
//   AuthzProbe request <输出文件>   申请这个权限项（弹授权框），把系统授权的外部形式（32 字节，十六进制）写进 0600 的文件，
//                                  然后保持授权会话，直到标准输入读到一行（让核验在这期间进行）
//   AuthzProbe verify <外部形式> [权限项]   （以 root，模拟 abroadd）不带交互地核验外部形式是否拥有权限项
//   AuthzProbe remove              （以 root）删除这个权限项
import Foundation
import Security

let right = "com.claude-abroad.sensitive"
let prompt = "“Claude 出国”要执行一项敏感操作（M5-1 探查用，不会改动网络）。"

func fail(_ msg: String, _ st: OSStatus? = nil) -> Never {
    var s = msg
    if let st { s += "（OSStatus \(st)：\((SecCopyErrorMessageString(st, nil) as String?) ?? "")）" }
    FileHandle.standardError.write(Data((s + "\n").utf8))
    exit(1)
}

func makeRef() -> AuthorizationRef {
    var ref: AuthorizationRef?
    let st = AuthorizationCreate(nil, nil, [], &ref)
    guard st == errAuthorizationSuccess, let ref else { fail("AuthorizationCreate 失败", st) }
    return ref
}

/// 申请或核验一个权限项。
func copyRight(_ ref: AuthorizationRef, _ name: String, interactive: Bool) -> OSStatus {
    return name.withCString { cname in
        var item = AuthorizationItem(name: cname, valueLength: 0, value: nil, flags: 0)
        return withUnsafeMutablePointer(to: &item) { ip in
            var rights = AuthorizationRights(count: 1, items: ip)
            var flags: AuthorizationFlags = [.extendRights]
            if interactive { flags.insert(.interactionAllowed); flags.insert(.preAuthorize) }
            return AuthorizationCopyRights(ref, &rights, nil, flags, nil)
        }
    }
}

let args = CommandLine.arguments
guard args.count >= 2 else { fail("用法：AuthzProbe register|show|request <文件>|verify <外部形式> [权限项]|remove") }

switch args[1] {
case "register":
    // 规则：用户本人（会话所有者）认证；shared=false：每个授权会话各自认证，别的进程不能沾光；timeout=0：不缓存。
    let rule: [String: Any] = [
        "class": "user",
        "authenticate-user": true,
        "session-owner": true,
        "shared": false,
        "timeout": 0,
        "version": 1,
        "comment": "Claude 出国的敏感操作（M5-1 探查）",
    ]
    let ref = makeRef()
    let st = AuthorizationRightSet(ref, right, rule as CFDictionary, prompt as CFString, nil, nil)
    guard st == errAuthorizationSuccess else { fail("登记权限项失败", st) }
    print("已登记权限项 \(right)（本人认证、不共享、不缓存）")
case "show":
    var dict: CFDictionary?
    let st = AuthorizationRightGet(right, &dict)
    guard st == errAuthorizationSuccess, let d = dict as? [String: Any] else { fail("读权限项失败（可能还没登记）", st) }
    for k in ["class", "authenticate-user", "session-owner", "shared", "timeout"].sorted() { print("\(k) = \(d[k].map { "\($0)" } ?? "（没有）")") }
case "request":
    guard args.count == 3 else { fail("用法：AuthzProbe request <输出文件>") }
    let ref = makeRef()
    let st = copyRight(ref, right, interactive: true)
    guard st == errAuthorizationSuccess else { fail("申请权限项失败（取消了授权框，或没有登记）", st) }
    var ext = AuthorizationExternalForm()
    let st2 = AuthorizationMakeExternalForm(ref, &ext)
    guard st2 == errAuthorizationSuccess else { fail("生成外部形式失败", st2) }
    let hex = withUnsafeBytes(of: &ext.bytes) { $0.map { String(format: "%02x", $0) }.joined() }
    let old = umask(0o077)
    FileManager.default.createFile(atPath: args[2], contents: Data((hex + "\n").utf8))
    umask(old)
    print("授权成功，外部形式已写到 \(args[2])；保持授权会话，按回车结束")
    _ = readLine()
    AuthorizationFree(ref, [.destroyRights])
    print("授权会话已结束")
case "verify":
    guard args.count >= 3 else { fail("用法：AuthzProbe verify <外部形式> [权限项]") }
    let name = args.count >= 4 ? args[3] : right
    let hex = args[2].trimmingCharacters(in: .whitespacesAndNewlines)
    guard hex.count == 64 else { fail("外部形式应是 64 个十六进制字符") }
    var ext = AuthorizationExternalForm()
    var i = hex.startIndex
    withUnsafeMutableBytes(of: &ext.bytes) { buf in
        for k in 0..<32 {
            let j = hex.index(i, offsetBy: 2)
            buf[k] = UInt8(hex[i..<j], radix: 16) ?? 0
            i = j
        }
    }
    var ref: AuthorizationRef?
    let st = AuthorizationCreateFromExternalForm(&ext, &ref)
    guard st == errAuthorizationSuccess, let ref else { print("核验：不通过（外部形式无效，OSStatus \(st)）"); exit(3) }
    let st2 = copyRight(ref, name, interactive: false)
    if st2 == errAuthorizationSuccess { print("核验：通过（\(name)）") } else { print("核验：不通过（\(name)，OSStatus \(st2)）"); exit(3) }
case "remove":
    let ref = makeRef()
    let st = AuthorizationRightRemove(ref, right)
    guard st == errAuthorizationSuccess else { fail("删除权限项失败（可能本来就没有）", st) }
    print("已删除权限项 \(right)")
default:
    fail("不认识的子命令 \(args[1])")
}
