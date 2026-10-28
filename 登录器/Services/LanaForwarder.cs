using System;
using System.Collections.Generic;
using System.Net;
using System.Net.Sockets;
using System.Text;
using System.Threading;
using System.Threading.Tasks;

namespace DNFLogin.Services
{
    /// <summary>
    /// 90 远程模式本地转发器：DNF.exe 连本机端口，转发器连远程服务器 7001，
    /// 先完成 LANA 握手（收 challenge → 回 ticket），再双向透传 DNF 协议。
    /// 每个登录会话（每个 DNF.exe 进程）一个实例，持有各自 ticket，天然隔离双开账号。
    /// </summary>
    public sealed class LanaForwarder : IDisposable
    {
        private const byte FrameVersion = 1;
        private static readonly byte[] Magic = { (byte)'L', (byte)'A', (byte)'N', (byte)'A' };

        private readonly string _serverHost;
        private readonly int _serverPort;
        private readonly string _ticket;
        private readonly string _machineId;
        private TcpListener? _listener;
        private CancellationTokenSource? _cts;
        private readonly List<Task> _sessions = new();
        private bool _disposed;

        public int LocalPort { get; private set; }

        public LanaForwarder(string serverHost, int serverPort, string ticket, string machineId)
        {
            _serverHost = serverHost;
            _serverPort = serverPort;
            _ticket = ticket ?? "";
            var mid = machineId ?? "";
            if (mid.Length > 128)
            {
                mid = mid.Substring(0, 128);
            }
            _machineId = mid;
        }

        /// <summary>监听 127.0.0.1 随机端口，返回实际监听端口号。</summary>
        public int Start()
        {
            if (_listener is not null)
            {
                throw new InvalidOperationException("forwarder already started");
            }
            _cts = new CancellationTokenSource();
            _listener = new TcpListener(IPAddress.Loopback, 0);
            _listener.Start();
            LocalPort = ((IPEndPoint)_listener.LocalEndpoint).Port;
            _ = Task.Run(() => AcceptLoopAsync(_cts.Token));
            return LocalPort;
        }

        private async Task AcceptLoopAsync(CancellationToken ct)
        {
            while (!ct.IsCancellationRequested && _listener is not null)
            {
                TcpClient local;
                try
                {
                    local = await _listener.AcceptTcpClientAsync(ct).ConfigureAwait(false);
                }
                catch (OperationCanceledException)
                {
                    break;
                }
                catch (ObjectDisposedException)
                {
                    break;
                }
                catch (SocketException)
                {
                    break;
                }

                var task = Task.Run(() => HandleSessionAsync(local, ct), ct);
                lock (_sessions)
                {
                    _sessions.Add(task);
                }
            }
        }

        private async Task HandleSessionAsync(TcpClient local, CancellationToken ct)
        {
            TcpClient? remote = null;
            try
            {
                remote = new TcpClient();
                remote.NoDelay = true;
                local.NoDelay = true;
                using var connectTimeout = CancellationTokenSource.CreateLinkedTokenSource(ct);
                connectTimeout.CancelAfter(TimeSpan.FromSeconds(5));
                await remote.ConnectAsync(_serverHost, _serverPort, connectTimeout.Token).ConfigureAwait(false);

                var remoteStream = remote.GetStream();
                var localStream = local.GetStream();

                // 1) 读服务端 challenge：header(7) + body
                var header = await ReadExactAsync(remoteStream, 7, ct).ConfigureAwait(false);
                if (!StartsWith(header, Magic) || header[4] != FrameVersion)
                {
                    throw new InvalidOperationException("invalid LANA challenge header");
                }
                int bodyLen = (header[5] << 8) | header[6];
                if (bodyLen < 0 || bodyLen > 4096)
                {
                    throw new InvalidOperationException("invalid LANA challenge length");
                }
                var challenge = await ReadExactAsync(remoteStream, bodyLen, ct).ConfigureAwait(false);
                if (challenge.Length < 20)
                {
                    throw new InvalidOperationException("invalid LANA challenge body");
                }

                // 2) 回 auth 帧：ticket(64 hex) + machine_id
                var ticketBytes = Encoding.ASCII.GetBytes(_ticket);
                var machineBytes = Encoding.ASCII.GetBytes(_machineId);
                var authBody = new byte[ticketBytes.Length + machineBytes.Length];
                Buffer.BlockCopy(ticketBytes, 0, authBody, 0, ticketBytes.Length);
                Buffer.BlockCopy(machineBytes, 0, authBody, ticketBytes.Length, machineBytes.Length);

                var frame = new byte[7 + authBody.Length];
                Buffer.BlockCopy(Magic, 0, frame, 0, 4);
                frame[4] = FrameVersion;
                frame[5] = (byte)(authBody.Length >> 8);
                frame[6] = (byte)(authBody.Length & 0xFF);
                Buffer.BlockCopy(authBody, 0, frame, 7, authBody.Length);
                await remoteStream.WriteAsync(frame, 0, frame.Length, ct).ConfigureAwait(false);

                // 3) 双向透传
                var up = PipeAsync(localStream, remoteStream, ct);
                var down = PipeAsync(remoteStream, localStream, ct);
                await Task.WhenAny(up, down).ConfigureAwait(false);
            }
            catch (OperationCanceledException) { }
            catch (ObjectDisposedException) { }
            catch (Exception)
            {
                // 握手/转发失败：直接断开本地连接，游戏表现为连接失败。
            }
            finally
            {
                try { remote?.Close(); } catch { }
                try { local.Close(); } catch { }
            }
        }

        private static async Task PipeAsync(NetworkStream from, NetworkStream to, CancellationToken ct)
        {
            var buf = new byte[64 * 1024];
            while (!ct.IsCancellationRequested)
            {
                int n = await from.ReadAsync(buf, 0, buf.Length, ct).ConfigureAwait(false);
                if (n <= 0)
                {
                    return;
                }
                await to.WriteAsync(buf, 0, n, ct).ConfigureAwait(false);
                await to.FlushAsync(ct).ConfigureAwait(false);
            }
        }

        private static async Task<byte[]> ReadExactAsync(NetworkStream stream, int count, CancellationToken ct)
        {
            var buf = new byte[count];
            int off = 0;
            while (off < count)
            {
                int n = await stream.ReadAsync(buf, off, count - off, ct).ConfigureAwait(false);
                if (n <= 0)
                {
                    throw new InvalidOperationException("unexpected EOF in LANA handshake");
                }
                off += n;
            }
            return buf;
        }

        private static bool StartsWith(byte[] data, byte[] prefix)
        {
            if (data.Length < prefix.Length)
            {
                return false;
            }
            for (int i = 0; i < prefix.Length; i++)
            {
                if (data[i] != prefix[i])
                {
                    return false;
                }
            }
            return true;
        }

        public void Dispose()
        {
            if (_disposed)
            {
                return;
            }
            _disposed = true;
            try { _cts?.Cancel(); } catch { }
            try { _listener?.Stop(); } catch { }
            try { _cts?.Dispose(); } catch { }
            _listener = null;
        }
    }
}
