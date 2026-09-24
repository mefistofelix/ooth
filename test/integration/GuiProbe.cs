using System;
using System.IO;
using System.Windows.Forms;

class GuiProbe : Form {
    readonly string marker;
    GuiProbe(string path) {
        marker = path;
        ShowInTaskbar = false;
        Opacity = 0;
        WindowState = FormWindowState.Minimized;
        Shown += (sender, args) => File.WriteAllText(marker, "ready");
        FormClosing += (sender, args) => File.WriteAllText(marker, "closed");
    }
    [STAThread]
    static void Main(string[] args) { Application.Run(new GuiProbe(args[0])); }
}
