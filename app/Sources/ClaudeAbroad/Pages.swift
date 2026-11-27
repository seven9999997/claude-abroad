// 其余页面（M5-9）共用的部分：加载状态、页面底部的提示和终端退路。各页面在 ExitsPage.swift、AppsPage.swift、EnvPage.swift、
// RegionPage.swift、EnginesPage.swift。文字都来自 L10n。
import AbroadAPI
import AbroadKit
import SwiftUI

/// 加载状态的统一显示：后台不可达、不提供、加载失败都有说明和“重试”，不出现空白页。
struct LoadStateView<Content: View>: View {
    let state: LoadState
    let retry: () -> Void
    @ViewBuilder let content: () -> Content

    var body: some View {
        switch state {
        case .loading:
            VStack(spacing: 12) {
                ProgressView()
                    .controlSize(.regular)
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .background(Palette.window)
        case .loaded:
            content()
        case .unreachable:
            message(L10n.unreachableTitle, L10n.backendDownDetail)
        case .unsupported(let t):
            message(L10n.unsupportedTitle, t.raw)
        case .failed(let t):
            PageScroll {
                ErrorBlock(text: t)
                HStack {
                    Button(action: retry) {
                        Label(L10n.retry, systemImage: "arrow.clockwise")
                    }
                    .buttonStyle(SecondaryButtonStyle())
                    Spacer()
                }
            }
        }
    }

    private func message(_ title: String, _ detail: String) -> some View {
        PageScroll {
            Banner(title: title, detail: detail, symbol: "exclamationmark.circle")
            HStack {
                Button(action: retry) {
                    Label(L10n.retry, systemImage: "arrow.clockwise")
                }
                .buttonStyle(SecondaryButtonStyle())
                Spacer()
            }
        }
    }
}

/// 页面共用的提示、终端退路、确认框。
struct PageChrome: ViewModifier {
    @ObservedObject var model: PageModel
    func body(content: Content) -> some View {
        content
            .safeAreaInset(edge: .bottom) {
                if let n = model.notice {
                    ErrorBlock(text: n.text)
                        .frame(maxWidth: 860)
                        .padding(.horizontal, 28)
                        .padding(.bottom, 14)
                }
            }
            .sheet(isPresented: Binding(get: { model.terminal != nil }, set: { if !$0 { Task { await model.dismissTerminal() } } })) {
                if let t = model.terminal { TerminalSheet(prompt: t) { Task { await model.dismissTerminal() } } }
            }
    }
}
