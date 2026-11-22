package virtualnetwork

// Close 关闭虚拟网络的 gVisor 协议栈并等它的协程全部退出（claude-abroad 补丁 0007）。
// 上游没有这个方法：协议栈和进程同生命周期时不需要；但同一进程里反复创建（测试）时，
// 没关闭的协议栈会留下分发协程，在竞态检测器下持续空转。调用前虚拟机端的连接应已关闭。
func (n *VirtualNetwork) Close() {
	n.stack.Close()
	n.stack.Wait()
}
