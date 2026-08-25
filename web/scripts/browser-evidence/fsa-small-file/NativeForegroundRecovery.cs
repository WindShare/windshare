using System;
using System.ComponentModel;
using System.Runtime.InteropServices;

namespace WindShare.FsaEvidence
{
    public static class NativeForegroundRecoveryV1
    {
        private const int ShowRestore = 9;

        [DllImport("user32.dll", SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        private static extern bool IsWindow(IntPtr window);

        [DllImport("user32.dll")]
        private static extern IntPtr GetForegroundWindow();

        [DllImport("user32.dll")]
        private static extern uint GetWindowThreadProcessId(IntPtr window, out uint processId);

        [DllImport("kernel32.dll")]
        private static extern uint GetCurrentThreadId();

        [DllImport("user32.dll", SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        private static extern bool AttachThreadInput(uint attach, uint attachTo, bool value);

        [DllImport("user32.dll")]
        [return: MarshalAs(UnmanagedType.Bool)]
        private static extern bool ShowWindow(IntPtr window, int command);

        [DllImport("user32.dll", SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        private static extern bool BringWindowToTop(IntPtr window);

        [DllImport("user32.dll", SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        private static extern bool SetForegroundWindow(IntPtr window);

        public static void Recover(long rootHandle)
        {
            var root = new IntPtr(rootHandle);
            if (!IsWindow(root))
            {
                throw new InvalidOperationException("The verified native picker no longer exists");
            }

            uint targetThread = GetWindowThreadProcessId(root, out _);
            uint currentThread = GetCurrentThreadId();
            var foreground = GetForegroundWindow();
            uint foregroundThread = foreground == IntPtr.Zero
                ? currentThread
                : GetWindowThreadProcessId(foreground, out _);
            bool targetAttached = currentThread == targetThread ||
                AttachThreadInput(currentThread, targetThread, true);
            if (!targetAttached)
            {
                throw new Win32Exception(Marshal.GetLastWin32Error(), "Could not attach to the picker input queue");
            }
            bool foregroundAttached = foregroundThread == currentThread || foregroundThread == targetThread ||
                AttachThreadInput(currentThread, foregroundThread, true);
            if (!foregroundAttached)
            {
                if (currentThread != targetThread) AttachThreadInput(currentThread, targetThread, false);
                throw new Win32Exception(Marshal.GetLastWin32Error(), "Could not attach to the foreground input queue");
            }
            try
            {
                ShowWindow(root, ShowRestore);
                BringWindowToTop(root);
                if (!SetForegroundWindow(root) || GetForegroundWindow() != root)
                {
                    throw new InvalidOperationException("Could not recover verified picker foreground ownership");
                }
            }
            finally
            {
                if (foregroundThread != currentThread && foregroundThread != targetThread)
                {
                    AttachThreadInput(currentThread, foregroundThread, false);
                }
                if (currentThread != targetThread) AttachThreadInput(currentThread, targetThread, false);
            }
        }
    }
}
