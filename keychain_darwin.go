//go:build darwin && cgo

package main

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>
static char* cursor_keychain_token(void) {
 const void* keys[] = { kSecClass, kSecAttrService, kSecAttrAccount, kSecReturnData, kSecMatchLimit };
 const void* values[] = { kSecClassGenericPassword, CFSTR("cursor-access-token"), CFSTR("cursor-user"), kCFBooleanTrue, kSecMatchLimitOne };
 CFDictionaryRef query=CFDictionaryCreate(NULL,keys,values,5,&kCFTypeDictionaryKeyCallBacks,&kCFTypeDictionaryValueCallBacks);
 CFTypeRef result=NULL;OSStatus status=SecItemCopyMatching(query,&result);CFRelease(query);
 if(status!=errSecSuccess||result==NULL)return NULL;
 if(CFGetTypeID(result)!=CFDataGetTypeID()){CFRelease(result);return NULL;}
 CFIndex len=CFDataGetLength((CFDataRef)result);char* out=(char*)malloc((size_t)len+1);
 if(out!=NULL){memcpy(out,CFDataGetBytePtr((CFDataRef)result),(size_t)len);out[len]=0;}
 CFRelease(result);return out;
}
*/
import "C"
import "unsafe"

func readKeychainToken() (string, error) {
	token := C.cursor_keychain_token()
	if token == nil {
		return "", nil
	}
	defer C.free(unsafe.Pointer(token))
	return C.GoString(token), nil
}
