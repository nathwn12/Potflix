package me.potflix.rpc;

import com.del.potplayercontrol.api.JNAPotPlayerHelper;
import com.del.potplayercontrol.api.PlayStatus;
import com.del.potplayercontrol.api.PotPlayer;
import com.del.potplayercontrol.impl.JNAPotPlayer;
import com.del.potplayercontrol.impl.Window;
import com.sun.jna.platform.WindowUtils;
import com.sun.jna.platform.win32.User32;
import de.jcm.discordgamesdk.Core;
import de.jcm.discordgamesdk.CreateParams;
import de.jcm.discordgamesdk.activity.Activity;
import de.jcm.discordgamesdk.activity.ActivityType;
import me.ayunami2000.pprpc.DownloadNativeLibrary;

import javax.imageio.ImageIO;
import java.awt.*;
import java.io.File;
import java.time.Instant;
import java.util.List;
import java.util.Objects;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * Potflix - Netflix-styled Discord presence for PotPlayer.
 * Fork of ayunami2000/PotPlayer-DiscordRPC: same engine, cleaner titles,
 * WATCHING activity, dark status window, red-on-black branding.
 */
public class Main {

    static java.net.ServerSocket lock; // single-instance guard, held for life
    // headless: no UI state kept.


    static java.io.PrintWriter dbg;
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
    public static void main(String[] args) throws Exception {
        try {
            lock = new java.net.ServerSocket(17843, 10, java.net.InetAddress.getByName("127.0.0.1"));
        } catch (Exception e) {
            System.err.println("Potflix is already running.");
            return;
        }
        buildTray();

        long clientId = resolveClientId();
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
    }

    // ---------- presence loop (same engine as upstream) ----------

    static void pollLoop(Core core) throws InterruptedException {
        String appName = "PotPlayer";
        String end = " - " + appName;
        PotPlayer player;
        boolean cleared = true;
        while (true) {
            List<Window> windows = JNAPotPlayerHelper.getAllPlayerWindows(
                    window -> window.getWindowText().endsWith(end) || window.getWindowText().equals(appName));
            if (!windows.isEmpty()) {
                cleared = false;
                player = new JNAPotPlayer(windows.get(0));
                if (player.getPlayStatus() == PlayStatus.Undefined) {
                    Thread.sleep(160);
                    continue;
                }
                // One anchor is held for the whole play session; it is re-anchored only
                // on a real seek (per-poll ct jump > SEEK_JUMP_MS) or a title/state
                // change. Paused sends exactly one frozen update, then stays silent.
                PresenceTracker tracker = new PresenceTracker();
                while (User32.INSTANCE.IsWindow(player.getWindow().getHwnd())) {
                    String tmp = WindowUtils.getWindowTitle(player.getWindow().getHwnd());
                    if (!tmp.endsWith(end) && !tmp.equals(appName)) break;
                    PlayStatus tmpps = player.getPlayStatus();
                    long tmpct = player.getCurrentTime();
                    long now = System.currentTimeMillis();
                    PresenceAction action = tracker.poll(tmp, tmpps, tmpct, now);
                    if (action != null) {
                        dlog("decision ms=" + now + " st=" + tmpps + " ct=" + tmpct + " title=[" + tmp + "] action=" + describe(action));
                        pushActivity(core, player, tmp, end, action);
                    }
                    core.runCallbacks();
                    Thread.sleep(250);
                }
                core.activityManager().clearActivity();
                dlog("cleared (player window gone)");
                setUi("Waiting for PotPlayer\u2026", "Idle", "", 0, 1);
            } else {
                if (!cleared) {
                    core.activityManager().clearActivity();
                    cleared = true;
                    dlog("cleared (no PotPlayer)");
                }
                setUi("Waiting for PotPlayer\u2026", "Idle", "", 0, 1);
            }
            Thread.sleep(1000);
        }
    }

    static void pushActivity(Core core, PotPlayer player, String rawTitle, String end, PresenceAction a) {
        String raw = rawTitle;
        if (raw.endsWith(end)) raw = raw.substring(0, raw.lastIndexOf(end));
        if (raw.trim().isEmpty() || raw.equals("PotPlayer")) {
            try (Activity activity = new Activity()) {
                activity.setType(ActivityType.PLAYING);
                activity.assets().setLargeImage("logo");
                activity.assets().setLargeText("");
                activity.setDetails("Browsing");
                activity.setState("Idle");
                activity.assets().setSmallImage("stop");
                activity.assets().setSmallText("");
                core.activityManager().updateActivity(activity);
            }
            setUi("Waiting for PotPlayer\u2026", "Idle", "", 0, 1);
            return;
        }
        Cleaned cleaned = clean(raw);
        long total = player.getTotalTime();
        try (Activity activity = new Activity()) {
            activity.setType(ActivityType.PLAYING);
            activity.assets().setLargeImage("logo");
            activity.assets().setLargeText("");
            String details = trunc(cleaned.title, 120);
            activity.setDetails(details);
            String context = "";
            if (!cleaned.episodeTag.isEmpty()) {
                context = cleaned.episodeTag
                        + (cleaned.episodeName.isEmpty() ? "" : " \"" + trunc(cleaned.episodeName, 60) + "\"");
            }
            context = trunc(context, 120);
            // Headless rows already carry the episode tag in details.
            // Netflix-native: text carries title+episode only; elapsed+remaining
            // come from Discord native timers (start+end), never duplicated.
            // Paused carries no timer at all, so its frozen position rides the text.
            activity.setState(withStateTime(context.equals(details) ? "" : context, a.stateSuffix));
            if (a.hasStart) {
                activity.timestamps().setStart(Instant.ofEpochMilli(a.start));
                activity.assets().setSmallImage("play");
            } else if (a.paused) {
                activity.assets().setSmallImage("pause");
            } else {
                activity.assets().setSmallImage("stop");
            }
            activity.assets().setSmallText("");
            core.activityManager().updateActivity(activity);
            dlog("push details=[" + activity.getDetails() + "] state=[" + activity.getState() + "] ct=" + a.ct + " total=" + total);
        }
        String st = a.paused ? "Paused" : a.running ? "Playing" : "Stopped";
        setUi(cleaned.display, st,
                (a.running || a.paused) ? fmt(a.ct) + " / " + fmt(total) : "",
                (int) Math.min(a.ct, Integer.MAX_VALUE), (int) Math.max(total, 1));
    }

    // ---------- presence timing/state logic (pure: no Discord, unit-testable) ----------

    /** Per-poll ct jump above this is a real seek; normal playback advance is far smaller. */
    public static final long SEEK_JUMP_MS = 1500;

    /** ct advance across the window that proves the position is really moving (settles a flapping status read). */
    public static final long SETTLE_MOVE_MS = 50;

    /** ct samples kept to prove real movement; a single coarse delta is not enough (reviewer F2). */
    private static final int MOVE_WINDOW = 3;

    /** One decision from {@link PresenceTracker}: whether/how to update Discord. */
    public static final class PresenceAction {
        public final boolean running;
        public final boolean paused;
        public final boolean hasStart;
        public final long start;          // epoch millis, meaningful when hasStart
        public final String stateSuffix;  // "" or " - MM:SS" (paused only)
        public final long ct;
        PresenceAction(boolean running, boolean paused, boolean hasStart, long start, String stateSuffix, long ct) {
            this.running = running;
            this.paused = paused;
            this.hasStart = hasStart;
            this.start = start;
            this.stateSuffix = stateSuffix;
            this.ct = ct;
        }
    }

    /** Compact one-line rendering of a poll decision for the debug log (null when nothing was sent). */
    static String describe(PresenceAction a) {
        if (a == null) return "null";
        String tag = a.running ? "RUN" : a.paused ? "PAUSE" : "STOP";
        return tag + " running=" + a.running
                + " paused=" + a.paused
                + " hasStart=" + a.hasStart
                + " start=" + a.start
                + " suf=" + a.stateSuffix
                + " ct=" + a.ct;
    }

    /**
     * Discord interpolates its timer from ONE anchor. Re-anchoring on every poll made
     * the timer jump ahead then snap back, so the anchor is held and re-taken only when
     * a valid ct jumps by more than SEEK_JUMP_MS (a real seek) or the title/play state
     * changes. A not-ready read (ct<=0) is ignored, so it neither erases the seek
     * baseline (F1) nor anchors zero; movement is proven across a short window because
     * ct ticks are coarse (F2); Undefined carries no information and changes nothing.
     * Paused holds a frozen position in text (" - MM:SS") and emits nothing further
     * until a title change or a paused seek.
     */
    public static final class PresenceTracker {
        private String lastTitle;
        private PlayStatus lastStatus;
        private long prevCt = -1;         // last VALID ct (used only as the seek baseline)
        private long frozenCt = -1;       // position held in the paused text
        private boolean havePrev;
        private final long[] ctWindow = new long[MOVE_WINDOW];
        private int ctWindowLen = 0;
        private int ctWindowIdx = 0;

        private void sampleCt(long ct) {
            ctWindow[ctWindowIdx] = ct;
            ctWindowIdx = (ctWindowIdx + 1) % MOVE_WINDOW;
            if (ctWindowLen < MOVE_WINDOW) ctWindowLen++;
        }
        private long oldestCt() {
            long m = Long.MAX_VALUE;
            for (int i = 0; i < ctWindowLen; i++) if (ctWindow[i] < m) m = ctWindow[i];
            return m;
        }

        public PresenceAction poll(String rawTitle, PlayStatus ps, long ct, long now) {
            // New media: forget everything.
            if (havePrev && !Objects.equals(lastTitle, rawTitle)) {
                havePrev = false; lastStatus = null; frozenCt = -1; prevCt = -1;
                ctWindowLen = 0; ctWindowIdx = 0;
            }
            // A not-ready read (ct<=0) must NOT overwrite the seek baseline, or a later
            // real jump can never be seen again (reviewer F1).
            boolean ctOk = ct > 0;
            long prevCtBefore = prevCt;
            if (ctOk) { sampleCt(ct); prevCt = ct; }
            // ct ticks are coarse (~500 ms), so one poll delta is not enough proof of
            // movement; require progress across a short window (reviewer F2).
            boolean moving = ctOk && ctWindowLen >= 2 && (ct - oldestCt()) >= SETTLE_MOVE_MS;

            // Undefined carries no information: never emit or change state on it.
            if (ps == PlayStatus.Undefined) { havePrev = true; return null; }

            // A non-Running report while the position is still moving, having settled into
            // Running, is a transient glitch: the player is really playing.
            PlayStatus eff = ps;
            if (ps != PlayStatus.Running && moving && lastStatus == PlayStatus.Running) eff = PlayStatus.Running;

            boolean changed = !havePrev || eff != lastStatus;
            boolean seeked = !changed && eff == PlayStatus.Running && lastStatus == PlayStatus.Running
                    && havePrev && ctOk && Math.abs(ct - prevCtBefore) > SEEK_JUMP_MS;
            boolean pausedSeek = !changed && eff == PlayStatus.Paused && lastStatus == PlayStatus.Paused
                    && frozenCt >= 0 && ctOk && Math.abs(ct - frozenCt) > SEEK_JUMP_MS;

            PresenceAction action = null;
            if (eff == PlayStatus.Running) {
                if ((changed || seeked) && ctOk) action = new PresenceAction(true, false, true, now - ct, "", ct);
            } else if (eff == PlayStatus.Paused) {
                if ((changed || pausedSeek) && ctOk) {
                    action = new PresenceAction(false, true, false, 0, " - " + fmt(ct), ct);
                    frozenCt = ct;
                }
            } else if (changed) {
                action = new PresenceAction(false, false, false, 0, "", ct);
            }

            if (action != null) { lastTitle = rawTitle; lastStatus = eff; }
            havePrev = true;
            return action;
        }
    }

    /** Compose the activity state line: context plus an optional frozen " - MM:SS". */
    public static String withStateTime(String context, String suffix) {
        if (suffix == null || suffix.isEmpty()) return context == null ? "" : context;
        if (context == null || context.isEmpty()) return suffix.replaceFirst("^\\s*-\\s*", "");
        return trunc(context + suffix, 128);
    }

    // ---------- title cleaner: raw filename -> Netflix-style display ----------

    static final Pattern EXT = Pattern.compile("\\.(mp4|mkv|avi|webm|ts|m2ts|mov|wmv|flv|mpg|mpeg|m4v|ass|srt)$", Pattern.CASE_INSENSITIVE);
    // P2: allow an empty series head so "S01E03-name" still parses; widen season to 4 digits (S1950E04).
    static final Pattern SERIES = Pattern.compile("(?i)^(.*?)[. _\\-]*[Ss](\\d{1,4})[ . _\\-]*[Ee](\\d{1,3})(?:[. _\\-]+(.*))?$");
    // P3: double episode, tested before SERIES ("…S02E13E14 name", "…S03E30-E31 name").
    static final Pattern MULTI = Pattern.compile("(?i)^(.*?)[. _\\-]*[Ss](\\d{1,4})[ . _\\-]*[Ee](\\d{1,3})[ ._\\-]*[-\\u2013\\u2014]?[Ee](\\d{1,3})(?:[. _\\-]+(.*))?$");
    // P5: "Season NN Episode MM" -> S(N) E(M); "Season NN <tail>" drops the connector.
    static final Pattern SEASON_EP = Pattern.compile("(?i)^(.*?)[. _\\-]*Season[ . _\\-]*(\\d{1,2})[. _\\-]+(?:Episode|Ep)\\.?[ . _\\-]*(\\d{1,4})(?:[. _\\-]+(.*))?$");
    static final Pattern SEASON_EXTRA = Pattern.compile("(?i)^(.*?)[. _\\-]*Season[ . _\\-]*(\\d{1,2})[. _\\-]+(.+)$");
    // P6: movie/anime "Chapter NN" (only when the name carries no 4-digit year).
    static final Pattern CHAPTER = Pattern.compile("(?i)^(.+?)[. _\\-]+Chapter[ . _\\-]*(\\d{1,4})(?:[. _\\-]+(.*))?$");
    // P4: headless "Episode NN"/"Ep NN".
    static final Pattern EPWORD = Pattern.compile("(?i)^(.*?)[. _\\-]*(?:Episode|Ep)\\.?[ . _\\-]*(\\d{1,4})(?:[. _\\-]+(.*))?$");
    // P7: "NNxNN".
    static final Pattern XEP = Pattern.compile("(?i)^(.*?)[. _\\-]+(\\d{1,2})x(\\d{2,3})(?:[. _\\-]+(.*))?$");
    // P8: conservative trailing episode number (0-prefixed or 3-digit), no 4-digit year.
    static final Pattern TRAIL = Pattern.compile("(?i)^(.{3,}?)[. _\\-]+(0\\d{1,2}|\\d{3})$");
    static final Pattern ALT_EP = Pattern.compile("(?i)^(.+?)[. _\\-]+(?:EP?|#)?[ . _\\-]*(\\d{1,3})(?:[. _\\-]+(.*))?$");
    static final Pattern FANSUB = Pattern.compile("(?i)^(.+?)\\s*[-;]\\s*(?:Ep?\\.?\\s*)?(\\d{1,4})(?:\\s*[-;]\\s*(.*))?$");
    // OP/ED bookkeeping: the head is optional so headless "ED 03"/"OP 02"/"NCOP01" parse too.
    static final Pattern NCBOOK = Pattern.compile("(?i)^(?:(.*?)[. _\\-]+)?(?:NC)?(OP|ED)[ . _\\-]*(\\d{1,3})(?:\\s*[-;]\\s*(.*))?$");
    // P10: added codec/source/language tokens; bare extended/theatrical/final removed.
    static final Pattern TAGS = Pattern.compile("(?i)\\b(2160p|1080p|720p|480p|360p|4k|uhd|hdr\\d*\\+?|dolby[ .\\-]?vision|blu[ .\\-]?ray|bdrip|brrip|web[ .\\-]?dl|webrip|web[ .\\-]?hd|hdtv|dvdrip|dvdscr|camrip|hdts|hdcam|hdrip|h[ .\\-]?26[45]|x26[45]|xvid|hevc|av1|vp9|10bit|8bit|aac\\d?(?:[ .\\-]?\\d)?|ac3|eac3|dts(?:[ .\\-]?hd)?|ddp?a?[ .\\-]?\\d(?:[ .\\-]?\\d)?|truehd|atmos|flac|mp3|opus|dual(?:[ .\\-]?audio)?|multi[ .\\-]?subs?|multi\\d*|msubs|subbed|dubbed|extended[ .\\-]?cut|unrated|theatrical[ .\\-]?cut|directors?[ .\\-]?cut|repack|proper|nf|amzn|dsnp|atvp|hmax|imax|sdr|hlg|vmaf\\d*|thai|tamil|hindi|kor|jpn|eng|rarbg|rartv|ettv|yts|yify|mkvcage|psa|evo|lol|ion10|vostfr|vost|5[ .\\-]?1|7[ .\\-]?1)\\b");
    // P11: added av1/opus/4k/uhd so "-GROUP" suffixes on AV1/Opus releases get dropped.
    static final Pattern SCENE_HINT = Pattern.compile("(?i)(1080p|2160p|720p|4k|uhd|x26[45]|web[ .\\-]?dl|bluray|bdrip|hdtv|dvdrip|hevc|av1|opus)");
    static final Pattern YEAR = Pattern.compile("^(.*)[. _\\-]+(19\\d{2}|20\\d{2})$");

    static class Cleaned {
        String title = "";
        String display = "";
        String episodeTag = "";
        String episodeName = "";
    }

    static Cleaned clean(String raw) {
        Cleaned out = new Cleaned();
        String s = EXT.matcher(raw.trim()).replaceAll("");
        s = s.replaceAll("^\\s*\\[[^\\]]+]\\s*", ""); // [Group] prefix
        s = s.replaceAll("\\s*\\[[A-Fa-f0-9]{8}]\\s*$", ""); // [CRC32] suffix
        boolean hasYear = s.matches("(?i).*\\b(19|20)\\d{2}\\b.*");
        // P1: drop parenthetical tags, collapse, and strip trailing separators so the
        // NCOP/NCED "$" anchor survives names like "…_-_NCOP01_(10bit…)".
        String ds = s.replaceAll("\\([^)]*\\)", " ").replaceAll("\\s+", " ").trim()
                .replaceAll("[. _\\-]+$", "");
        Matcher nc = NCBOOK.matcher(ds);
        if (nc.matches()) {
            Cleaned ncout = new Cleaned();
            String nhead = (nc.group(1) == null) ? "" : nc.group(1).replaceAll("(?i)[. _\\-]+S\\d{1,2}$", "");
            ncout.episodeTag = ("OP".equalsIgnoreCase(nc.group(2)) ? "OP " : "ED ") + Integer.parseInt(nc.group(3));
            ncout.episodeName = (nc.group(4) == null) ? "" : stripTags(nc.group(4));
            String ntitle = stripTags(nhead);
            // P12: headless OP/ED (no name) shows the tag alone, not "ED 3 — ED 3".
            if (ntitle.isEmpty()) {
                if (!ncout.episodeName.isEmpty()) { ntitle = ncout.episodeName; ncout.episodeName = ""; }
                else ntitle = ncout.episodeTag;
            }
            ncout.title = trunc(ntitle, 100);
            ncout.display = buildDisplay(ncout.title, ncout.episodeTag, ncout.episodeName);
            return ncout;
        }
        // scene release group suffix ("\u2026x264-GalaxyRG"): drop trailing -GROUP only when
        // the name carries codec tags, so real titles like "Spider-Man" survive
        if (SCENE_HINT.matcher(s).find()) s = s.replaceAll("[-_.][A-Za-z0-9]{2,12}$", "");

        String head = null, epTag = "", rest = null;
        boolean got = false;
        Matcher m;
        // order matters: multi-ep -> season -> chapter -> series -> fansub -> epword -> xep -> trail -> alt
        m = MULTI.matcher(s);
        if (m.matches()) {
            head = m.group(1);
            epTag = "S" + Integer.parseInt(m.group(2)) + " E" + Integer.parseInt(m.group(3)) + "-" + Integer.parseInt(m.group(4));
            rest = m.group(5); got = true;
        }
        if (!got) {
            m = SEASON_EP.matcher(s);
            if (m.matches()) { head = m.group(1); epTag = "S" + Integer.parseInt(m.group(2)) + " E" + Integer.parseInt(m.group(3)); rest = m.group(4); got = true; }
        }
        if (!got && !hasYear) {
            m = CHAPTER.matcher(s);
            if (m.matches()) { head = m.group(1); epTag = "E" + Integer.parseInt(m.group(2)); rest = m.group(3); got = true; }
        }
        if (!got) {
            m = SEASON_EXTRA.matcher(s);
            if (m.matches()) {
                Matcher tn = Pattern.compile("^(\\d{1,3})(?:[. _\\-]+(.*))?$").matcher(m.group(3));
                if (tn.matches()) {
                    head = m.group(1); epTag = "S" + Integer.parseInt(m.group(2)) + " E" + Integer.parseInt(tn.group(1)); rest = tn.group(2); got = true;
                } else {
                    String ttl = (stripTags(m.group(1)) + " " + stripTags(m.group(3))).trim();
                    out.title = trunc(ttl.isEmpty() ? stripTags(s) : ttl, 100);
                    out.display = out.title;
                    return out;
                }
            }
        }
        if (!got) {
            m = SERIES.matcher(s);
            if (m.matches()) { head = m.group(1); epTag = "S" + Integer.parseInt(m.group(2)) + " E" + Integer.parseInt(m.group(3)); rest = m.group(4); got = true; }
        }
        if (!got) {
            m = FANSUB.matcher(s);
            if (m.matches()) {
                int fnum = Integer.parseInt(m.group(2));
                boolean yearLike = m.group(2).length() == 4 && fnum >= 1900 && fnum <= 2100;
                if (!yearLike) { head = m.group(1); epTag = "E" + fnum; rest = m.group(3); got = true; }
            }
        }
        if (!got) {
            m = EPWORD.matcher(s);
            if (m.matches()) { head = m.group(1); epTag = "E" + Integer.parseInt(m.group(2)); rest = m.group(3); got = true; }
        }
        if (!got) {
            m = XEP.matcher(s);
            if (m.matches()) { head = m.group(1); epTag = "S" + Integer.parseInt(m.group(2)) + " E" + Integer.parseInt(m.group(3)); rest = m.group(4); got = true; }
        }
        if (!got && !hasYear) {
            m = TRAIL.matcher(s);
            if (m.matches()) { head = m.group(1); epTag = "E" + Integer.parseInt(m.group(2)); rest = null; got = true; }
        }
        if (!got) {
            Matcher a = ALT_EP.matcher(s);
            // P9: reject connector-headed heads ("…Chapter/Part/Season") and year-leading tails
            // so movie "Part/Chapter N" is not stolen as an episode.
            if (a.matches() && a.group(1).length() >= 3 && a.group(2).length() <= 3 && a.group(3) != null && a.group(3).length() < 40
                    && s.matches("(?i).*[. _\\-](EP?|#)?[ . _\\-]*\\d{1,3}([. _\\-].*)?$") && stripTags(s).length() > a.group(1).length()
                    && !a.group(1).matches("(?i).*\\b(Season|Chapter|Part|Episode|Ep|Vol(ume)?)$")
                    && !a.group(3).matches("^(19|20)\\d{2}\\b.*")) {
                head = a.group(1); epTag = "E" + Integer.parseInt(a.group(2)); rest = a.group(3); got = true;
            }
        }
        String core = got ? stripTags(head) : stripTags(s);
        String epName = (got && rest != null) ? stripTags(rest) : "";
        // Uppercase release groups can remain even without codec/source hints.
        epName = epName.replaceAll("\\s*-\\s*[A-Z0-9]{2,}$", "").trim();
        // Drop observed release metadata, not legitimate single-word episode titles.
        // Numeric tails are episode/version fragments; '+' can survive HDR10+ stripping.
        if (epName.matches("(?i)(?:WEB|DS4K|OVA|v\\d+|\\d+|\\+)")) epName = "";
        if (got && core.isEmpty()) {
            out.episodeTag = epTag;
            if (!epName.isEmpty()) { // P12: use the episode name as the head
                out.title = trunc(epName, 100);
                out.episodeName = "";
                out.display = out.title + " \u2014 " + epTag;
                return out;
            }
            out.title = epTag; // P12: show the tag alone
            out.episodeName = "";
            out.display = epTag;
            return out;
        }
        if (core.isEmpty()) core = stripTags(s);
        out.title = trunc(core, 100);
        out.episodeTag = epTag;
        out.episodeName = epName;
        out.display = buildDisplay(out.title, out.episodeTag, out.episodeName);
        return out;
    }

    static String buildDisplay(String title, String tag, String name) {
        if (tag == null || tag.isEmpty()) return title;
        if (title == null || title.isEmpty()) return tag;
        return title + " \u2014 " + tag + (name == null || name.isEmpty() ? "" : " \"" + trunc(name, 60) + "\"");
    }

    static String stripTags(String s) {
        String r = s.replaceAll("[._]+", " ");
        r = r.replaceAll("\\[.*?\\]", " ").replaceAll("\\(.*?\\)", " ");
        r = TAGS.matcher(r).replaceAll(" ");
        r = r.replaceAll("\\s+", " ").trim();
        r = r.replaceAll("^[\\-_. ]+", "").replaceAll("[\\-_. ]+$", "");
        Matcher y = YEAR.matcher(r);
        String year = "";
        if (y.matches()) {
            r = y.group(1).trim();
            year = " (" + y.group(2) + ")";
        }
        if (!r.isEmpty()) r = r.substring(0, 1).toUpperCase() + r.substring(1);
        return (r + year).trim();
    }

    static String fmt(long ms) {
        long seconds = ms / 1000;
        long s = seconds % 60;
        long m = (seconds / 60) % 60;
        long h = seconds / 3600;
        if (h == 0) return String.format("%02d:%02d", m, s);
        return String.format("%d:%02d:%02d", h, m, s);
    }

    static String trunc(String s, int n) {
        if (s == null) return "";
        return s.length() <= n ? s : s.substring(0, n - 1).trim() + "\u2026";
    }

    static long resolveClientId() {
        long def = 1146489019925004359L;
        String env = System.getenv("POTFLIX_CLIENT_ID");
        if (env != null) {
            try {
                return Long.parseLong(env.trim());
            } catch (NumberFormatException ignored) {
            }
        }
        for (String p : new String[]{"client-id.txt", "config.properties"}) {
            try {
                File f = new File(p);
                if (f.exists()) {
                    String t = new String(java.nio.file.Files.readAllBytes(f.toPath())).trim();
                    Matcher d = Pattern.compile("(\\d{17,20})").matcher(t);
                    if (d.find()) return Long.parseLong(d.group(1));
                }
            } catch (Exception ignored) {
            }
        }
        return def;
    }

    // ---------- Netflix-styled tray + status window ----------

    static void buildTray() throws Exception {
        if (!SystemTray.isSupported()) return;
        PopupMenu popup = new PopupMenu();
        MenuItem exit = new MenuItem("Exit Potflix");
        exit.addActionListener(e -> System.exit(0));
        popup.add(exit);
        TrayIcon icon = new TrayIcon(ImageIO.read(Objects.requireNonNull(Main.class.getResource("/icon.png"))), "Potflix", popup);
        icon.setImageAutoSize(true);
        try {
            SystemTray.getSystemTray().add(icon);
        } catch (AWTException e) {
            System.err.println("TrayIcon could not be added.");
        }
    }

    // headless: status sink so polling code stays untouched (JITs away).
    static void setUi(String title, String status, String time, int cur, int max) { }
}
