// M5-1 探查 E4、E5：经 unix socket 读 abroadd 的 status（用生成的 Codable 类型解码）和事件流（SSE）。
//   UDSClient status <abroadd 的 api.sock>
//   UDSClient events <事件服务的 socket>
import APITypes
import Foundation
import UDSHTTP

let args = CommandLine.arguments
guard args.count == 3 else {
    FileHandle.standardError.write(Data("用法：UDSClient status|events <socket>\n".utf8))
    exit(2)
}
let client = UnixHTTPClient(path: args[2])
switch args[1] {
case "status":
    do {
        let r = try await client.request(path: "/v1/status")
        let st = try JSONDecoder().decode(DaemonStatus.self, from: r.body)
        print("HTTP \(r.status)；状态 \(st.state)；闸门 \(st.gateOpen ? "开" : "关")；事件 \(st.events?.count ?? 0) 条；用生成的类型解码成功")
    } catch {
        print("读 status 失败：\(error)")
        exit(1)
    }
case "events":
    final class State: @unchecked Sendable { var parser = SSEParser(); var n = 0; var failed = false }
    let s = State()
    await withCheckedContinuation { (cont: CheckedContinuation<Void, Never>) in
    client.stream(path: "/v1/events", onHead: { code, h in
        print("HTTP \(code)；Content-Type \(h["content-type"] ?? "")；分块传输 \(h["transfer-encoding"] ?? "否")")
    }, onBody: { chunk in
        for ev in s.parser.feed(chunk) {
            s.n += 1
            if let d = ev.data.data(using: .utf8), let pe = try? JSONDecoder().decode(ProbeProbeEvent.self, from: d) {
                print("事件 \(ev.id ?? "-") \(ev.event)：seq \(pe.seq) 状态 \(pe.state ?? "")")
            } else {
                print("事件 \(ev.id ?? "-") \(ev.event)：解不出数据"); s.failed = true
            }
        }
    }, done: { err in
        if let err { print("事件流出错：\(err)"); s.failed = true }
        cont.resume()
    })
    }
    print("共收到 \(s.n) 个事件")
    exit(s.failed || s.n == 0 ? 1 : 0)
default:
    exit(2)
}
