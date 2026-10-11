-- 025 gave existing messages their sender's time, which a clock running ahead put in the future; nothing was received after this upgrade.
UPDATE messages SET received_at = unixepoch() * 1000 WHERE received_at > unixepoch() * 1000;
