// 两步确认的预览和终端退路的提示。
import AbroadKit
import SwiftUI

struct ConfirmSheet: View {
    let prompt: ConfirmPrompt
    let answer: (Bool) -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(L10n.confirmTitle).font(.headline)
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
            HStack {
                Spacer()
                Button(L10n.cancel) { answer(false) }.keyboardShortcut(.cancelAction)
                Button(L10n.confirm) { answer(true) }
            }
        }
        .padding(20)
        .frame(minWidth: 460)
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
