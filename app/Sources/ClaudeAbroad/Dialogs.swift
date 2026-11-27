// 两步确认的预览和终端退路的提示（样式照原型 ui/design/index.html 的 .sheet：应用图标加标题、说明、分组框、底部按钮）。
import AbroadKit
import SwiftUI

/// 统一的确认框（m5-plan v2.9 决定 2）：内容全部来自后台的预览；“确认”出现 3 秒后才可点；显示剩余有效时间，到期后只能关掉。
struct ConfirmSheet: View {
    @ObservedObject var flow: ConfirmFlow

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            SheetHeader(title: L10n.confirmTitle, symbol: "exclamationmark")
            if let prompt = flow.prompt {
                if flow.refreshed { ToneNote(tone: .attention, text: L10n.confirmRefreshed, symbol: "arrow.clockwise") }
                MessageText(text: prompt.message)
                if prompt.returnBlock { // M6-11b：回国后保留回国拦截
                    ToneNote(tone: .attention, text: L10n.returnBlockConsequence, symbol: "hand.raised")
                }
                let procs = prompt.preview.processes ?? []
                CardSection(nil) {
                    if let r = prompt.region {
                        CardRow {
                            RowLabel(title: L10n.regionChange)
                            Text(L10n.regionFromTo(r.from, r.to)).foregroundStyle(.secondary)
                        }
                        RowSeparator()
                    }
                    CardRow {
                        RowLabel(title: L10n.processesWillQuit, bold: true)
                        Text(L10n.processCount(procs.count)).foregroundStyle(.secondary)
                    }
                    ForEach(procs, id: \.pid) { p in
                        RowSeparator()
                        CardRow { Text("\(p.pid)  \(p.app)  \(p.cmd)").font(.callout.monospaced()).textSelection(.enabled) }
                    }
                }
                if let un = prompt.preview.unverified, !un.isEmpty {
                    CardSection(L10n.processesUnverified) {
                        ForEach(Array(un.enumerated()), id: \.element.pid) { i, p in
                            if i > 0 { RowSeparator() }
                            CardRow { Text("\(p.pid)  \(p.app)  \(p.note ?? "")").font(.callout.monospaced()).foregroundStyle(.secondary) }
                        }
                    }
                }
                if let bs = prompt.preview.browsers, !bs.isEmpty { // 回国：浏览器总是列出；勾了才结束确认过的（M6-4b-3）
                    CardSection(L10n.browsersRunning) {
                        ForEach(bs, id: \.pid) { b in
                            CardRow { Text("\(b.pid)  \(b.app)  \(b.note ?? "")").font(.callout.monospaced()) }
                            RowSeparator()
                        }
                        CardRow { Toggle(L10n.killBrowsersOption, isOn: $flow.killBrowsers).disabled(flow.submitting) }
                    }
                }
                status
            }
            if let n = flow.notice { ErrorBlock(text: n) }
            if flow.submitting { HStack { ProgressView().controlSize(.small); Text(L10n.confirmSubmitting) } }
            SheetFooter {
                Spacer()
                Button(L10n.cancel) { flow.cancel() }.keyboardShortcut(.cancelAction).disabled(flow.submitting) // 请求已发出：不能取消
                Button(L10n.confirm) { Task { await flow.confirm() } }
                    .buttonStyle(.borderedProminent)
                    .disabled(flow.step(at: flow.now) != .ready || flow.submitting)
            }
            .controlSize(.large)
        }
        .padding(.horizontal, 20)
        .padding(.top, 18)
        .padding(.bottom, 16)
        .frame(width: 540)
        .tint(Palette.accent)
        .background(Palette.window)
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
        case .expired: ToneNote(tone: .failure, text: L10n.confirmExpired, symbol: "clock.badge.xmark")
        case .waiting:
            let left = max(1, Int(ceil(ConfirmFlow.delay - flow.now.timeIntervalSince(flow.shownAt ?? flow.now))))
            Text(L10n.confirmWait(left)).font(.caption).foregroundStyle(.secondary)
        case .ready: Text(L10n.confirmRemaining(flow.remaining(at: flow.now))).font(.caption).foregroundStyle(.secondary)
        case .none: EmptyView()
        }
    }
}

struct TerminalSheet: View {
    let prompt: TerminalPrompt
    let close: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            SheetHeader(title: L10n.terminalTitle, symbol: "terminal")
            if let f = prompt.tempFile {
                Text(L10n.terminalStepWrite).bold()
                Text(L10n.terminalTempFile).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                CodeBlock(text: f.path)
                Text(L10n.terminalStepRun).bold()
            } else {
                Text(L10n.terminalDetail).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            ForEach(prompt.commands, id: \.self) { c in
                CodeBlock(text: c)
            }
            SheetFooter {
                Spacer()
                Button(L10n.copyCommand) {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(prompt.commands.joined(separator: "\n"), forType: .string)
                }
                Button(L10n.terminalDone) { close() }.keyboardShortcut(.defaultAction)
            }
            .controlSize(.large)
        }
        .padding(.horizontal, 20)
        .padding(.top, 18)
        .padding(.bottom, 16)
        .frame(width: 520)
        .tint(Palette.accent)
        .background(Palette.window)
    }
}
