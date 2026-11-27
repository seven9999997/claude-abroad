// B0（v0.71b）：全应用唯一能提权的地方（源码检查守着）。用系统自带的 AppleScript“do shell script … with administrator
// privileges”弹系统密码框，以 root 执行 PrivilegedCommand 拼好的命令（运行的是已安装的、属主 root 的命令行）。
// - 基础路径只在这里出现一次（下面的常量），安装目录、命令行、临时目录都从它拼出；
// - 在自己的后台串行队列上执行（界面先显示“正在执行，请输入管理员密码”，不卡界面）；不用 osascript 子进程；
// - 同一进程认证一次后约 5 分钟内再执行不再弹密码框（系统行为）；每次执行前都有应用自己的确认框，只有用户点“执行”才走到这里；
// - 只回传标准输出和取消、失败；不写日志。
import AbroadKit
import Foundation

final class PrivilegedRunner: PrivilegedRunning, @unchecked Sendable {
    /// 唯一的基础路径常量。
    private static let base = "/Library/Application Support"
    private let queue = DispatchQueue(label: "claude-abroad.privileged", qos: .userInitiated)

    let paths = PrivilegedPaths(installDir: PrivilegedRunner.base + "/claude-abroad", staging: PrivilegedRunner.base + "/claude-abroad-staging")

    func run(command: String, prompt: String) async -> PrivilegedRunResult {
        // 两层转义：命令里的参数已经是单引号（PrivilegedCommand），这里再按 AppleScript 字符串转义。不改行尾（输出按 \n 分行解析）。
        let source = "do shell script \"" + PrivilegedCommand.appleScriptEscape(command) + "\" with prompt \"" +
            PrivilegedCommand.appleScriptEscape(prompt) + "\" with administrator privileges without altering line endings"
        return await withCheckedContinuation { cont in
            queue.async {
                guard let script = NSAppleScript(source: source) else {
                    cont.resume(returning: .failed(L10n.privScriptFailed))
                    return
                }
                var err: NSDictionary?
                let out = script.executeAndReturnError(&err)
                if let err {
                    let num = (err[NSAppleScript.errorNumber] as? Int) ?? 0
                    if num == -128 { cont.resume(returning: .cancelled); return }
                    cont.resume(returning: .failed((err[NSAppleScript.errorMessage] as? String) ?? L10n.privErrorNumber(num)))
                    return
                }
                cont.resume(returning: .finished(stdout: out.stringValue ?? ""))
            }
        }
    }
}
