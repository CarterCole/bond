package com.cartertech.bond.ui

import android.content.Context
import com.google.mlkit.vision.barcode.common.Barcode
import com.google.mlkit.vision.codescanner.GmsBarcodeScannerOptions
import com.google.mlkit.vision.codescanner.GmsBarcodeScanning
import com.cartertech.bond.R

/**
 * Scans a QR code with Google's code scanner: it runs in Google Play services, shows its own camera
 * screen and hands back only the text, so this app needs no camera permission.
 *
 * It fails where Play services is missing, and until Play services has fetched its scanner module
 * (asked for at install time, see the manifest). Backing out of the scanner reports nothing.
 */
fun scanQrCode(context: Context, onCode: (String) -> Unit, onError: (String) -> Unit) {
    val options = GmsBarcodeScannerOptions.Builder()
        .setBarcodeFormats(Barcode.FORMAT_QR_CODE)
        .build()
    GmsBarcodeScanning.getClient(context, options).startScan()
        .addOnSuccessListener { barcode -> barcode.rawValue?.let(onCode) }
        .addOnFailureListener { e ->
            onError(context.getString(R.string.scan_failed, (e.localizedMessage ?: e.toString()).trimEnd('.')))
        }
}
