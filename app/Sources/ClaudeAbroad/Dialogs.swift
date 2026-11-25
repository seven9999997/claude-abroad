// 两步确认的预览和终端退路的提示。
import AbroadKit
import SwiftUI

/// 统一的确认框（m5-plan v2.9 决定 2）：内容全部来自后台的预览；“确认”出现 3 秒后才可点；显示剩余有效时间，到期后只能关掉。
struct ConfirmSheet: View {
    @ObservedObject var flow: ConfirmFlow

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(L10n.confirmTitle).font(.headline)
            if let prompt = flow.prompt {
                if flow.refreshed { Text(L10n.confirmRefreshed).foregroundStyle(.orange) }
                ErrorBlock(text: prompt.message)
                if let r = prompt.region {
                    Text(L10n.regionChange + "：" + L10n.regionFromTo(r.from, r.to))
                }
                let procs = prompt.preview.processes ?? []
                Text(L10n.processesWillQuit + "：" + L10n.processCount(procs.count))
                ForEach(procs, id: \.pid) { p in
                    Text("\(p.pid)  \(p.app)  \(p.cmd)").font(.callout.monospaced())
                }
                if let un = prompt.preview.unverified, !un.isEmpty {
                    Text(L10n.processesUnverified).foregroundStyle(.secondary)
                    ForEach(un, id: \.pid) { p in
                        Text("\(p.pid)  \(p.app)  \(p.note ?? "")").font(.callout.monospaced()).foregroundStyle(.secondary)
                    }
                }
                if let bs = prompt.preview.browsers, !bs.isEmpty { // 回国：浏览器总是列出；勾了才结束确认过的（M6-4b-3）
                    Text(L10n.browsersRunning)
                    ForEach(bs, id: \.pid) { b in
                        Text("\(b.pid)  \(b.app)  \(b.note ?? "")").font(.callout.monospaced())
                    }
                    Toggle(L10n.killBrowsersOption, isOn: $flow.killBrowsers).disabled(flow.submitting)
                }
                status
            }
            if let n = flow.notice { ErrorBlock(text: n) }
            if flow.submitting { HStack { ProgressView().controlSize(.small); Text(L10n.confirmSubmitting) } }
            HStack {
                Spacer()
                Button(L10n.cancel) { flow.cancel() }.keyboardShortcut(.cancelAction).disabled(flow.submitting) // 请求已发出：不能取消
                Button(L10n.confirm) { Task { await flow.confirm() } }
                    .disabled(flow.step(at: flow.now) != .ready || flow.submitting)
            }
        }
        .padding(20)
        .frame(minWidth: 460)
        .interactiveDismissDisabled(flow.submitting)
        .task {
            while !Task.isCancelled {
                flow.tick()
                try? await Task.sleep(nanoseconds: 500_000_000)
            }
        }
    }

    @ViewBuilder private var status: some View {
        switch flow.step(at: flow.now) {
        case .expired: Text(L10n.confirmExpired).foregroundStyle(.red)
        case .waiting:
            let left = max(1, Int(ceil(ConfirmFlow.delay - flow.now.timeIntervalSince(flow.shownAt ?? flow.now))))
            Text(L10n.confirmWait(left)).foregroundStyle(.secondary)
        case .ready: Text(L10n.confirmRemaining(flow.remaining(at: flow.now))).foregroundStyle(.secondary)
        case .none: EmptyView()
        }
    }
}

struct TerminalSheet: View {
    let prompt: TerminalPrompt
    let close: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(L10n.terminalTitle).font(.headline)
            if let f = prompt.tempFile {
                Text(L10n.terminalStepWrite).bold()
                Text(L10n.terminalTempFile)
                Text(f.path).font(.callout.monospaced()).textSelection(.enabled)
                Text(L10n.terminalStepRun).bold()
            } else {
                Text(L10n.terminalDetail)
            }
            ForEach(prompt.commands, id: \.self) { c in
                Text(c).font(.body.monospaced()).textSelection(.enabled)
                    .padding(8).background(RoundedRectangle(cornerRadius: 6).fill(Color.secondary.opacity(0.12)))
            }
            HStack {
                Spacer()
                Button(L10n.copyCommand) {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(prompt.commands.joined(separator: "\n"), forType: .string)
                }
                Button(L10n.terminalDone) { close() }.keyboardShortcut(.defaultAction)
            }
        }
        .padding(20)
        .frame(minWidth: 460)
    }
}
