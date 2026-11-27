// 两步确认的预览和终端退路的提示（样式照原型 ui/design/index.html 的 .sheet：应用图标加标题、说明、分组框、底部按钮）。
import AbroadKit
import AppKit
import SwiftUI

/// 统一的确认框（m5-plan v2.9 决定 2）：内容全部来自后台的预览；“确认”出现 3 秒后才可点；显示剩余有效时间，到期后只能关掉。
struct ConfirmSheet: View {
    @ObservedObject var flow: ConfirmFlow

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            SheetHeader(title: L10n.confirmTitle, symbol: "exclamationmark", tone: .attention)
            ScrollView {
                VStack(alignment: .leading, spacing: 12) {
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
                                InfoPill(text: L10n.processCount(procs.count), monospaced: true)
                            }
                            ForEach(procs, id: \.pid) { p in
                                RowSeparator()
                                CardRow(verticalPadding: 8) {
                                    InfoPill(text: "\(p.pid)", monospaced: true)
                                    Text("\(p.app)  \(p.cmd)")
                                        .font(.system(size: 12, design: .monospaced))
                                        .textSelection(.enabled)
                                }
                            }
                        }
                        if let un = prompt.preview.unverified, !un.isEmpty {
                            CardSection(L10n.processesUnverified) {
                                ForEach(Array(un.enumerated()), id: \.element.pid) { i, p in
                                    if i > 0 { RowSeparator() }
                                    CardRow(verticalPadding: 8) {
                                        InfoPill(text: "\(p.pid)", monospaced: true)
                                        Text("\(p.app)  \(p.note ?? "")")
                                            .font(.system(size: 12, design: .monospaced))
                                            .foregroundStyle(.secondary)
                                    }
                                }
                            }
                        }
                        if let bs = prompt.preview.browsers, !bs.isEmpty { // 回国：浏览器总是列出；勾了才结束确认过的（M6-4b-3）
                            CardSection(L10n.browsersRunning) {
                                ForEach(bs, id: \.pid) { b in
                                    CardRow(verticalPadding: 8) {
                                        InfoPill(text: "\(b.pid)", monospaced: true)
                                        Text("\(b.app)  \(b.note ?? "")").font(.system(size: 12, design: .monospaced))
                                    }
                                    RowSeparator()
                                }
                                CardRow { Toggle(L10n.killBrowsersOption, isOn: $flow.killBrowsers).disabled(flow.submitting) }
                            }
                        }
                        status
                    }
                    if let n = flow.notice { ErrorBlock(text: n) }
                    if flow.submitting {
                        HStack(spacing: 8) {
                            ProgressView().controlSize(.small)
                            Text(L10n.confirmSubmitting).foregroundStyle(.secondary)
                        }
                    }
                }
                .padding(.vertical, 2)
            }
            .frame(maxHeight: 400)

            SheetFooter {
                Spacer()
                Button(L10n.cancel) { flow.cancel() }
                    .buttonStyle(SecondaryButtonStyle())
                    .keyboardShortcut(.cancelAction)
                    .disabled(flow.submitting) // 请求已发出：不能取消
                Button(L10n.confirm) { Task { await flow.confirm() } }
                    .buttonStyle(PrimaryButtonStyle())
                    .disabled(flow.step(at: flow.now) != .ready || flow.submitting)
            }
        }
        .padding(.horizontal, 22)
        .padding(.top, 20)
        .padding(.bottom, 18)
        .frame(width: 560)
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
            ToneNote(tone: .attention, text: L10n.confirmWait(left), symbol: "hourglass")
        case .ready:
            ToneNote(tone: .local, text: L10n.confirmRemaining(flow.remaining(at: flow.now)), symbol: "clock")
        case .none: EmptyView()
        }
    }
}

struct TerminalSheet: View {
    let prompt: TerminalPrompt
    let close: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            SheetHeader(title: L10n.terminalTitle, symbol: "terminal")
            if let f = prompt.tempFile {
                Text(L10n.terminalStepWrite).fontWeight(.semibold)
                Text(L10n.terminalTempFile).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                CodeBlock(text: f.path)
                Text(L10n.terminalStepRun).fontWeight(.semibold)
            } else {
                Text(L10n.terminalDetail).foregroundStyle(.secondary).lineSpacing(1.5).fixedSize(horizontal: false, vertical: true)
            }
            ForEach(prompt.commands, id: \.self) { c in
                CodeBlock(text: c)
            }
            SheetFooter {
                Spacer()
                Button {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(prompt.commands.joined(separator: "\n"), forType: .string)
                } label: {
                    Label(L10n.copyCommand, systemImage: "doc.on.doc")
                }
                .buttonStyle(SecondaryButtonStyle())

                Button(L10n.terminalDone) { close() }
                    .buttonStyle(PrimaryButtonStyle())
                    .keyboardShortcut(.defaultAction)
            }
        }
        .padding(.horizontal, 22)
        .padding(.top, 20)
        .padding(.bottom, 18)
        .frame(width: 540)
        .tint(Palette.accent)
        .background(Palette.window)
    }
}

/// B0（v0.71b）：应用里经系统密码框执行敏感操作的确认框。写清要做什么、现状和后果、只读接口给的预览；
/// 只有点“执行”才调用提权（PrivilegedSession.userTappedExecute）；“在终端执行”是原来的退路。
struct PrivilegedSheet: View {
    @ObservedObject var session: PrivilegedSession

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            if let r = session.request {
                SheetHeader(title: r.title, symbol: "lock.shield")
                ScrollView {
                    VStack(alignment: .leading, spacing: 12) {
                        if !r.lines.isEmpty {
                            CardSection(nil) {
                                ForEach(Array(r.lines.enumerated()), id: \.offset) { i, l in
                                    if i > 0 { RowSeparator() }
                                    CardRow(verticalPadding: 8) {
                                        Text(l).fixedSize(horizontal: false, vertical: true)
                                    }
                                }
                            }
                        }
                        if let b = r.blocked { ToneNote(tone: .failure, text: b, symbol: "xmark.octagon") }
                        preview(r)
                        if r.claudeProxy != nil { claudeProxy } // v0.72：回国确认里选第四道
                        if !r.removes.isEmpty { // 阶段 B：卸载会删除的东西（只读卸载预览给的）
                            CardSection(L10n.serviceRemoves) {
                                ForEach(Array(r.removes.enumerated()), id: \.offset) { i, x in
                                    if i > 0 { RowSeparator() }
                                    CardRow(verticalPadding: 8) {
                                        Text(x).fixedSize(horizontal: false, vertical: true)
                                    }
                                }
                            }
                        }
                        if !r.inputFiles.isEmpty {
                            Text(L10n.privInputFile).font(.caption).foregroundStyle(.secondary)
                            ForEach(r.inputFiles, id: \.self) { CodeBlock(text: $0) }
                        }
                        if let n = r.note { Text(n).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true) }
                        result
                    }
                    .padding(.vertical, 2)
                }
                .frame(maxHeight: 420)

                SheetFooter {
                    if case .done = session.phase {
                        Spacer()
                        Button(L10n.privClose) { Task { await session.userClosed() } }
                            .buttonStyle(PrimaryButtonStyle())
                            .keyboardShortcut(.defaultAction)
                    } else {
                        Button(L10n.privInTerminal) { session.userTappedTerminal() }
                            .buttonStyle(SecondaryButtonStyle())
                            .disabled(session.phase == .running)
                        Spacer()
                        Button(L10n.cancel) { Task { await session.userClosed() } }
                            .buttonStyle(SecondaryButtonStyle())
                            .keyboardShortcut(.cancelAction)
                            .disabled(session.phase == .running)
                        Button(L10n.privExecute, action: { Task { await session.userTappedExecute() } })
                            .buttonStyle(PrimaryButtonStyle())
                            .disabled(session.phase != .confirming || r.blocked != nil || session.claudeProxyBlocked)
                    }
                }
            }
        }
        .padding(.horizontal, 22)
        .padding(.top, 20)
        .padding(.bottom, 18)
        .frame(width: 560)
        .tint(Palette.accent)
        .background(Palette.window)
        .interactiveDismissDisabled(session.phase == .running)
    }

    @ViewBuilder private func preview(_ r: PrivilegedRequest) -> some View {
        if r.region != nil || r.preview != nil {
            let procs = r.preview?.processes ?? []
            CardSection(nil) {
                if let g = r.region {
                    CardRow {
                        RowLabel(title: L10n.regionChange)
                        Text(L10n.regionFromTo(g.from, g.to)).foregroundStyle(.secondary)
                    }
                    RowSeparator()
                }
                CardRow {
                    RowLabel(title: L10n.processesWillQuit, bold: true)
                    InfoPill(text: L10n.processCount(procs.count), monospaced: true)
                }
                ForEach(procs, id: \.pid) { p in
                    RowSeparator()
                    CardRow(verticalPadding: 8) {
                        InfoPill(text: "\(p.pid)", monospaced: true)
                        Text("\(p.app)  \(p.cmd)")
                            .font(.system(size: 12, design: .monospaced))
                            .textSelection(.enabled)
                    }
                }
            }
            if let un = r.preview?.unverified, !un.isEmpty {
                CardSection(L10n.processesUnverified) {
                    ForEach(Array(un.enumerated()), id: \.element.pid) { i, p in
                        if i > 0 { RowSeparator() }
                        CardRow(verticalPadding: 8) {
                            InfoPill(text: "\(p.pid)", monospaced: true)
                            Text("\(p.app)  \(p.note ?? "")")
                                .font(.system(size: 12, design: .monospaced))
                                .foregroundStyle(.secondary)
                        }
                    }
                }
            }
            if r.browsersOption, let bs = r.preview?.browsers {
                CardSection(L10n.browsersRunning) {
                    ForEach(bs, id: \.pid) { b in
                        CardRow(verticalPadding: 8) {
                            InfoPill(text: "\(b.pid)", monospaced: true)
                            Text("\(b.app)  \(b.note ?? "")").font(.system(size: 12, design: .monospaced))
                        }
                        RowSeparator()
                    }
                    CardRow { Toggle(L10n.killBrowsersOption, isOn: $session.killBrowsers).disabled(session.phase != .confirming) }
                }
            }
        }
    }

    /// 回国确认里第四道的勾选（初值等于现状）和按勾选的说明；勾选打开且有冲突时列出冲突，“执行”不可点。
    @ViewBuilder private var claudeProxy: some View {
        CardSection(L10n.claudeCodeTitle) {
            CardRow { Toggle(L10n.restoreClaudeProxyOption, isOn: $session.claudeProxyOn).disabled(session.phase != .confirming) }
            ForEach(Array(session.claudeProxyLines.enumerated()), id: \.offset) { _, l in
                RowSeparator()
                CardRow { Text(l).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true) }
            }
        }
        if session.claudeProxyBlocked {
            ToneNote(tone: .failure, text: L10n.restoreClaudeProxyBlocked(session.claudeProxyConflicts), symbol: "xmark.octagon")
        }
    }

    @ViewBuilder private var result: some View {
        switch session.phase {
        case .running:
            HStack(spacing: 8) {
                ProgressView().controlSize(.small)
                Text(L10n.privRunning).foregroundStyle(.secondary)
            }
        case let .done(ok, message, output):
            if ok && session.warning { // 回国已完成、但第四道没有切换（v0.72）
                ToneNote(tone: .attention, text: message, symbol: "exclamationmark.triangle")
            } else if ok {
                ToneNote(tone: .success, text: message, symbol: "checkmark.circle")
            } else {
                ErrorBlock(text: ErrorText(raw: message))
            }
            if !output.isEmpty {
                Text(L10n.privOutput).font(.caption).foregroundStyle(.secondary)
                ScrollView { CodeBlock(text: output) }.frame(maxHeight: 160)
            }
        default:
            Text(L10n.privHow).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
    }
}
