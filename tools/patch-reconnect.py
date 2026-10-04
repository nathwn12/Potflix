import io

p = r'<workspace-root>\PERSONAL\Potflix\src\main\java\me\potflix\rpc\Main.java'
t = io.open(p, encoding='utf-8').read()
n0 = len(t)

def rep(old, new, tag):
    global t
    assert old in t, tag + ' NOT FOUND'
    t = t.replace(old, new, 1)

# 1. debug log + cached SDK helpers: insert before main()
rep('    public static void main(',
'''    static java.io.PrintWriter dbg;
    static synchronized void dlog(String s) {
        try {
            if (dbg == null) {
                File dir = new File(Main.class.getProtectionDomain().getCodeSource().getLocation().toURI()).getParentFile();
                dbg = new java.io.PrintWriter(new java.io.FileWriter(new File(dir, "potflix-debug.log"), true), true);
            }
            dbg.println(java.time.LocalTime.now().toString().substring(0, 8) + " " + s);
        } catch (Exception ignored) { }
    }
    static File discordLib() {
        try {
            File dir = new File(Main.class.getProtectionDomain().getCodeSource().getLocation().toURI()).getParentFile();
            File cached = new File(dir, "discord_game_sdk.dll");
            if (cached.exists() && cached.length() > 100000) return cached;
            File dl = DownloadNativeLibrary.downloadDiscordLibrary();
            if (dl == null) return null;
            java.nio.file.Files.copy(dl.toPath(), cached.toPath(), java.nio.file.StandardCopyOption.REPLACE_EXISTING);
            return cached;
        } catch (Exception e) {
            return null;
        }
    }
    public static void main(''', 'helpers')

# 2. session loop with reconnect
rep('''        long clientId = resolveClientId();
        File discordLibrary = DownloadNativeLibrary.downloadDiscordLibrary();
        if (discordLibrary == null) {
            System.err.println("Error downloading Discord SDK.");
            System.exit(-1);
            return;
        }
        Core.init(discordLibrary);
        try (CreateParams params = new CreateParams()) {
            params.setClientID(clientId);
            params.setFlags(CreateParams.getDefaultFlags());
            try (Core core = new Core(params)) {
                pollLoop(core);
            }
        }
    }''',
'''        long clientId = resolveClientId();
        while (true) {
            try {
                File discordLibrary = discordLib();
                if (discordLibrary == null) { Thread.sleep(10000); continue; }
                Core.init(discordLibrary);
                try (CreateParams params = new CreateParams()) {
                    params.setClientID(clientId);
                    params.setFlags(CreateParams.getDefaultFlags());
                    try (Core core = new Core(params)) {
                        dlog("connected clientId=" + clientId);
                        pollLoop(core);
                    }
                }
            } catch (Exception e) {
                dlog("session dropped: " + e);
            }
            try { Thread.sleep(5000); } catch (InterruptedException ie) { return; }
        }
    }''', 'session')

# 3. fast pickup when PotPlayer absent (latch speed over idle savings)
rep('            Thread.sleep(5000);', '            Thread.sleep(1000);', 'idle')

# 4. log every push (1Hz max while playing) + log clear
rep('            core.activityManager().updateActivity(activity);\n        }\n        String st = ps == PlayStatus.Running',
'''            core.activityManager().updateActivity(activity);
            dlog("push details=[" + activity.getDetails() + "] state=[" + activity.getState() + "] ct=" + ct + " total=" + total);
        }
        String st = ps == PlayStatus.Running''', 'pushlog')

io.open(p, 'w', encoding='utf-8', newline='').write(t)
print('reconnect patch ok, bytes %d -> %d' % (n0, len(t)))
