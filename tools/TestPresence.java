import me.potflix.rpc.Main;
import com.del.potplayercontrol.api.PlayStatus;

/**
 * Plain-JVM tests for the Potflix presence timing/state logic.
 * No Discord connection, no PotPlayer: drives Main.PresenceTracker with
 * synthetic (title, PlayStatus, ct, now) samples and inspects the push decisions.
 *
 *   javac -cp "<upstream.jar>;..\build\classes" -d ..\build\classes TestPresence.java
 *   java  -cp "..\build\classes;<upstream.jar>" TestPresence
 */
public class TestPresence {

    static int failures = 0;
    static final long T0 = 1_000_000L;

    static void check(boolean ok, String what) {
        System.out.println((ok ? "PASS " : "FAIL ") + what);
        if (!ok) failures++;
    }

    public static void main(String[] args) {
        String title = "Breaking.Bad.S02E08.1080p.WEB-DL.x264.mkv";
        Main.PresenceAction p;

        // ---------- (a) no re-anchor while ct advances normally (~2s across polls) ----------
        Main.PresenceTracker tr = new Main.PresenceTracker();
        int pushes = 0;
        Main.PresenceAction lastRunning = null;
        long now = T0, ct = 0;
        p = tr.poll(title, PlayStatus.Running, ct, now);
        if (p != null) { pushes++; lastRunning = p; }
        for (int i = 1; i <= 8; i++) {            // 8 polls x 250ms = ~2s of content
            ct += 250; now += 250;
            p = tr.poll(title, PlayStatus.Running, ct, now);
            if (p != null) { pushes++; lastRunning = p; }
        }
        check(pushes == 1, "(a) ~2s of normal advances -> exactly one push (got " + pushes + ")");
        check(lastRunning != null && lastRunning.hasStart && lastRunning.start == T0,
                "(a) one anchor held: start == now-ct at first push");

        // ---------- (b) re-anchor only on a per-poll jump > 1500ms ----------
        long seekCt = ct + 4000;                  // a real seek
        now += 250;
        p = tr.poll(title, PlayStatus.Running, seekCt, now);
        check(p != null, "(b) >1500ms jump -> push");
        check(p != null && p.hasStart && p.start == now - seekCt,
                "(b) seek re-anchors: start == now-ct");

        // boundary: exactly 1500ms is NOT a seek; 1501ms is.
        // Prime with a ready (ct>0) reading: ct==0 is a "not ready" poll that anchors
        // nothing (see case g), so it no longer establishes tracker state.
        Main.PresenceTracker tb = new Main.PresenceTracker();
        tb.poll(title, PlayStatus.Running, 30000, T0);
        p = tb.poll(title, PlayStatus.Running, 31500, T0 + 250);   // +1500
        check(p == null, "(b) 1500ms change is not a seek");
        p = tb.poll(title, PlayStatus.Running, 33001, T0 + 500);   // prev poll ct = 31500
        check(p != null, "(b) 1501ms change is a seek");

        // ---------- (c) paused: exactly one push, no timestamps, frozen ' - MM:SS' ----------
        tr = new Main.PresenceTracker();
        int pausedPushes = 0;
        p = tr.poll(title, PlayStatus.Running, 30000, T0);
        check(p != null && p.hasStart, "(c) running push carries an anchor");
        p = tr.poll(title, PlayStatus.Paused, 30000, T0 + 250);   // pause transition
        check(p != null && p.paused && !p.hasStart, "(c) paused push has NO timestamps");
        check(p != null && " - 00:30".equals(p.stateSuffix),
                "(c) frozen state suffix is ' - MM:SS' (got " + (p == null ? "null" : "[" + p.stateSuffix + "]") + ")");
        if (p != null && p.paused) pausedPushes++;
        for (int i = 1; i <= 5; i++) {            // paused: ct drifts a little, no repeats
            p = tr.poll(title, PlayStatus.Paused, 30000 + i * 200, T0 + 250 + i * 250);
            check(p == null, "(c) no push while paused (poll " + i + ")");
            if (p != null && p.paused) pausedPushes++;
        }
        check(pausedPushes == 1, "(c) exactly ONE paused push (got " + pausedPushes + ")");

        // ---------- (c2) paused seek refreshes the frozen text once ----------
        p = tr.poll(title, PlayStatus.Paused, 90000, T0 + 2000);
        check(p != null && p.paused && " - 01:30".equals(p.stateSuffix),
                "(c2) paused seek refreshes frozen text once");
        check(tr.poll(title, PlayStatus.Paused, 90000, T0 + 2250) == null,
                "(c2) refresh is not repeated");

        // ---------- state-line composition ----------
        check("S02 E08 - 00:30".equals(Main.withStateTime("S02 E08", " - 00:30")),
                "(c) state line = context + ' - MM:SS'");
        check("00:30".equals(Main.withStateTime("", " - 00:30")),
                "(c) empty context -> bare MM:SS (no dangling separator)");
        check("S02 E08".equals(Main.withStateTime("S02 E08", "")),
                "(c) running state line carries no time text");

        // ---------- (d) resume re-anchors ----------
        tr = new Main.PresenceTracker();
        tr.poll(title, PlayStatus.Running, 1000, T0);
        tr.poll(title, PlayStatus.Paused, 1000, T0 + 500);
        p = tr.poll(title, PlayStatus.Running, 1000, T0 + 1000);
        check(p != null && p.hasStart && p.start == (T0 + 1000) - 1000,
                "(d) resume re-anchors: start == now-ct");

        // ---------- (e) flapping status while playing is suppressed ----------
        // A transient/flapping getPlayStatus() between Running and Paused, while ct keeps
        // advancing, is NOT a real transition: the settle logic reads it as still playing.
        Main.PresenceTracker te = new Main.PresenceTracker();
        int eCount = 0;
        long eNow = T0, eCt = 1000;
        p = te.poll(title, PlayStatus.Running, eCt, eNow);
        check(p != null && p.hasStart, "(e) prime Running ct=1000 -> one push");
        Main.PresenceAction eAnchor = p;
        if (p != null) eCount++;
        eCt += 200; eNow += 250;
        p = te.poll(title, PlayStatus.Paused, eCt, eNow);
        check(p == null, "(e) flapping Paused ct=1200 suppressed");
        if (p != null) eCount++;
        eCt += 200; eNow += 250;
        p = te.poll(title, PlayStatus.Running, eCt, eNow);
        check(p == null, "(e) Running ct=1400 suppressed");
        if (p != null) eCount++;
        eCt += 200; eNow += 250;
        p = te.poll(title, PlayStatus.Paused, eCt, eNow);
        check(p == null, "(e) flapping Paused ct=1600 suppressed");
        if (p != null) eCount++;
        eCt += 200; eNow += 250;
        p = te.poll(title, PlayStatus.Running, eCt, eNow);
        check(p == null, "(e) Running ct=1800 suppressed");
        if (p != null) eCount++;
        check(eCount == 1, "(e) flapping stayed at ONE push (got " + eCount + ")");
        check(eAnchor != null && eAnchor.hasStart && eAnchor.start == T0 - 1000,
                "(e) original anchor/start unchanged");

        // ---------- (f) a real pause (ct frozen) is still detected ----------
        Main.PresenceTracker tf = new Main.PresenceTracker();
        p = tf.poll(title, PlayStatus.Running, 5000, T0);
        check(p != null && p.hasStart, "(f) prime Running ct=5000 -> one push");
        p = tf.poll(title, PlayStatus.Paused, 5000, T0 + 250);
        check(p != null && p.paused && !p.hasStart, "(f) real pause detected (ct did not move)");
        check(p != null && " - 00:05".equals(p.stateSuffix),
                "(f) frozen suffix is ' - 00:05' (got " + (p == null ? "null" : "[" + p.stateSuffix + "]") + ")");
        check(tf.poll(title, PlayStatus.Paused, 5000, T0 + 500) == null,
                "(f) repeat paused -> no push");

        // ---------- (g) ct==0 never anchors zero ----------
        Main.PresenceTracker tg = new Main.PresenceTracker();
        p = tg.poll(title, PlayStatus.Running, 0, T0);
        check(p == null, "(g) ct==0 -> no anchor (is a 'not ready' read)");
        p = tg.poll(title, PlayStatus.Running, 5000, T0 + 250);
        check(p != null && p.hasStart && p.start == (T0 + 250) - 5000,
                "(g) first real ct anchors now-ct");

        // ---------- (h) settled logic: flat-ct Paused/Running still transitions ----------
        Main.PresenceTracker th = new Main.PresenceTracker();
        p = th.poll(title, PlayStatus.Running, 1000, T0);
        check(p != null && p.hasStart, "(h) prime Running ct=1000 -> one push");
        int hCount = 0;
        p = th.poll(title, PlayStatus.Paused, 1000, T0 + 250);
        check(p != null && p.paused, "(h) Running->Paused with flat ct -> PAUSE");
        if (p != null) hCount++;
        p = th.poll(title, PlayStatus.Running, 1000, T0 + 500);
        check(p != null && p.hasStart && p.start == (T0 + 500) - 1000,
                "(h) Paused->Running re-anchors");
        if (p != null) hCount++;
        check(hCount == 2, "(h) exactly 2 transition pushes (PAUSE then RUN) (got " + hCount + ")");

        // ---------- (i) F1: a ct<=0 read must not erase the seek baseline ----------
        Main.PresenceTracker ti = new Main.PresenceTracker();
        p = ti.poll(title, PlayStatus.Running, 60000, T0);
        check(p != null && p.hasStart, "(i) prime Running ct=60000 -> RUN");
        p = ti.poll(title, PlayStatus.Running, 0, T0 + 250);
        check(p == null, "(i) ct==0 read is ignored, no push");
        p = ti.poll(title, PlayStatus.Running, 300, T0 + 500);
        check(p != null && p.hasStart && p.start == (T0 + 500) - 300,
                "(i) anchor re-taken after the not-ready read (start == now-ct)");

        // ---------- (j) F2: one small advance is not proof of movement ----------
        Main.PresenceTracker tj = new Main.PresenceTracker();
        int jCount = 0;
        p = tj.poll(title, PlayStatus.Running, 1000, T0);        if (p != null) jCount++;
        p = tj.poll(title, PlayStatus.Running, 1500, T0 + 250);  if (p != null) jCount++;
        p = tj.poll(title, PlayStatus.Running, 2000, T0 + 500);  if (p != null) jCount++;
        check(jCount == 1, "(j) prime 1000/1500/2000 -> one push (got " + jCount + ")");
        p = tj.poll(title, PlayStatus.Paused, 2000, T0 + 750);
        check(p == null, "(j) Paused with ct not advancing -> null");
        p = tj.poll(title, PlayStatus.Running, 2500, T0 + 1000);
        check(p == null, "(j) single small advance is not enough -> null");
        check(jCount == 1, "(j) total pushes stayed 1 (got " + jCount + ")");

        // ---------- (k) F4: ct<=0 on a real Paused report must not be a transition ----------
        Main.PresenceTracker tk = new Main.PresenceTracker();
        p = tk.poll(title, PlayStatus.Running, 5000, T0);
        check(p != null && p.hasStart, "(k) prime Running ct=5000 -> RUN");
        p = tk.poll(title, PlayStatus.Paused, -1, T0 + 250);
        check(p == null, "(k) Paused with ct<=0 (not ready) -> null");
        p = tk.poll(title, PlayStatus.Paused, 5000, T0 + 500);
        check(p != null && p.paused && " - 00:05".equals(p.stateSuffix),
                "(k) real pause after the bad read -> PAUSE ' - 00:05'");

        // ---------- (l) Undefined is ignored: never emits or changes state ----------
        Main.PresenceTracker tl = new Main.PresenceTracker();
        int lCount = 0;
        p = tl.poll(title, PlayStatus.Running, 1000, T0);        if (p != null) lCount++;
        p = tl.poll(title, PlayStatus.Running, 1500, T0 + 250);  if (p != null) lCount++;
        p = tl.poll(title, PlayStatus.Running, 2000, T0 + 500);  if (p != null) lCount++;
        p = tl.poll(title, PlayStatus.Undefined, 2000, T0 + 750);
        check(p == null, "(l) Undefined poll -> null");
        p = tl.poll(title, PlayStatus.Running, 2500, T0 + 1000);
        check(p == null, "(l) Running after Undefined stays settled -> null");
        check(lCount == 1, "(l) pushes stayed 1 (got " + lCount + ")");

        System.out.println(failures == 0 ? "ALL TESTS PASSED" : failures + " TEST(S) FAILED");
        if (failures != 0) System.exit(1);
    }
}
