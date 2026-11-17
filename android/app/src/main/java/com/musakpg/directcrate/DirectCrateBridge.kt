package com.musakpg.directcrate

object DirectCrateBridge {
    private var isLoaded = false

    fun loadNativeLibrary() {
        if (!isLoaded) {
            System.loadLibrary("directcrate")
            isLoaded = true
            syncTimezone()
        }
    }

    fun syncTimezone() {
        try {
            val tz = java.util.TimeZone.getDefault()
            val offsetSec = tz.getOffset(System.currentTimeMillis()) / 1000
            setTimezone(offsetSec, tz.id ?: "Local")
        } catch (ignored: Exception) {}
    }

    external fun setTimezone(offsetSeconds: Int, name: String)
    external fun startServer(configDir: String, defaultDest: String): Int
    external fun stopServer()
    external fun getProgressJson(): String
    external fun updateDeviceThermals(temp: Double, status: String)
    external fun getSessionToken(): String
    external fun abortTask()
}

