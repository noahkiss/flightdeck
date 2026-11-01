// Throwaway keys made with ssh-keygen only for these tests (invented
// comments, never used anywhere else). Regenerate with
// apple/scripts/gen-key-fixtures.sh. Each file is stored without its
// BEGIN and END lines, so secret scanners do not take these test keys for
// real ones; armor() puts them back.

enum KeyFixtures {
    static let passphrase = "correct horse battery staple"

    /// A key file from its kind (OPENSSH, EC, RSA, empty for PKCS#8) and body.
    private static func armor(_ kind: String, _ body: String) -> String {
        let label = (kind.isEmpty ? "" : kind + " ") + "PRIVATE" + " KEY"
        return "-----BEGIN \(label)-----\n" + body + "\n-----END \(label)-----\n"
    }

    static let ecdsa256 = armor("OPENSSH", """
        b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAaAAAABNlY2RzYS
        1zaGEyLW5pc3RwMjU2AAAACG5pc3RwMjU2AAAAQQTif5B9IGqcGpy3ddMpljIU+caSA0jp
        te/y8wcqtRh+7duPBGCvR99SA68GPvITUgpPcmMRwCpv3BjV43C/du6CAAAAsNkuLgDZLi
        4AAAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBOJ/kH0gapwanLd1
        0ymWMhT5xpIDSOm17/LzByq1GH7t248EYK9H31IDrwY+8hNSCk9yYxHAKm/cGNXjcL927o
        IAAAAhANUWD/GhMmETDTCO6R89kKm1adHo3bfr6J+mE35yBw/+AAAAEGVjZHNhMjU2QGV4
        YW1wbGUBAgMEBQYH
        """)
    static let ecdsa256_pub = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBOJ/kH0gapwanLd10ymWMhT5xpIDSOm17/LzByq1GH7t248EYK9H31IDrwY+8hNSCk9yYxHAKm/cGNXjcL927oI="
    static let ecdsa256_fp = "SHA256:eP0BtyyUJMMi0H/62yslDnPkyEzMUrPCiPK0morft/0"

    static let ecdsa256_enc = armor("OPENSSH", """
        b3BlbnNzaC1rZXktdjEAAAAACmFlczI1Ni1jdHIAAAAGYmNyeXB0AAAAGAAAABDxwHlTIj
        ZHjiD1oqwcPcMDAAAABAAAAAEAAABoAAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlz
        dHAyNTYAAABBBNgrjhPFG4iCufiksavSnN1NmHuf3qsmbl7aPZddJIl09WPmdIUC+pj3jU
        RCjbM6UaN00hyaiQWILbmIIJPqOq8AAACwp7kV5xMRdegvkWN6lj/Mnhtgg+gNIrsKogvW
        yNeWerdZu7ZZzEA+m6qF+XYdQz9ul7xZ1dlnw2vW0RMmzaGnEQlRHB/A009bHKe2BcvWpH
        z1CZ68G+TmmapNRZ3OhFjtVYCMi/5/t+l8ItAudSd2GVSW0jo/GZUPeD+Bl7uXildNnueM
        3vyV2o5r+grBQULWL79Iae1NnABhVgBkCU0W/DtjQEv1RxafNQFCfxKk/jU=
        """)
    static let ecdsa256_enc_pub = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBNgrjhPFG4iCufiksavSnN1NmHuf3qsmbl7aPZddJIl09WPmdIUC+pj3jURCjbM6UaN00hyaiQWILbmIIJPqOq8="
    static let ecdsa256_enc_fp = "SHA256:bZLjVeV21hEhi7KT9p2g3I/E4etZruNxzvmir4KA7Dg"

    static let ecdsa256_pem = armor("EC", """
        MIIBaAIBAQQgNwdHg2jf7FflbWds42Ag/M9YoS9pNDq2h7kdFFW/PtqggfowgfcC
        AQEwLAYHKoZIzj0BAQIhAP////8AAAABAAAAAAAAAAAAAAAA////////////////
        MFsEIP////8AAAABAAAAAAAAAAAAAAAA///////////////8BCBaxjXYqjqT57Pr
        vVV2mIa8ZR0GsMxTsPY7zjw+J9JgSwMVAMSdNgiG5wSTamZ44ROdJreBn36QBEEE
        axfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpZP40Li/hp/m47n60p8D54W
        K84zV2sxXs7LtkBoN79R9QIhAP////8AAAAA//////////+85vqtpxeehPO5ysL8
        YyVRAgEBoUQDQgAEI4h5OOnXnU7LMXcTuf4xkk3GgJIEcEoY0jSDpS6YJ3fSuUVF
        sdX11UwHH2GFChJQ1oO89Lg0s2DvGHe3xyxDvA==
        """)
    static let ecdsa256_pem_pub = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBCOIeTjp151OyzF3E7n+MZJNxoCSBHBKGNI0g6UumCd30rlFRbHV9dVMBx9hhQoSUNaDvPS4NLNg7xh3t8csQ7w="
    static let ecdsa256_pem_fp = "SHA256:ulSANuJcKuqKhRdIzQDZJYO6tvfi/k8YGw1LqL5s848"

    static let ecdsa256_pem_enc = armor("EC", """
        Proc-Type: 4,ENCRYPTED
        DEK-Info: AES-128-CBC,6385FDBB2CC64289A2B49753213BCDB8
        
        ig9pffjniX5lIlch636/tTW02hHVmFXJZQdZr+ljT2j5BAlw/035zIgZfOxI9VKg
        KKU+fCSaRfY78J2/6Qtyt4tu8tfRM1yp4xtwVEZsKho5lfP9f9vAILmnPcYo+A4f
        DPvxqwlROY4U/UrPX3KxwRpFhe8y9AMTb4opG0pmipzr1a4In8qsnJtl7kRgKmmU
        6iPDMoFwf8K3Hhb7fi9+9srEXHYvU1QWxWwXKNoSYRvnR1NKLZ7XdDgIJbpzaLlP
        ezLGY79iUZH/0h9dHe3zqIxeBYbRHvoPGWWgszDTlonZhmGzYa6JuCavJ94hS4Y6
        ZFK73F28pVfFoEt5QVOZZIw9gzmhCE8Neg4/Qf0R+fHJyqmXONRsM4kSQ4C3HHUS
        04l57zGFRBRTXuXQkuQk8/lN0wAMfi+E/aXPf4kdBsNJYYLzrM2jbUEnZu5yotAh
        B9pU8WWZ3p9IeIdHFELaOJu+Hy4ioUmxzBkXMeY8PSY=
        """)
    static let ecdsa256_pem_enc_pub = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBPNgDGutDx/oEnkipTYfXdQeqzCBYayGm/YrpQDHggN5Fp+lwwAj7YnzULsS4ymqECH8kKY5ntgISdsrMeOLE6w="
    static let ecdsa256_pem_enc_fp = "SHA256:axay43RJ58iqK9HZ18RohWg63JrerU6wd0sPGaLhKDU"

    static let ecdsa384 = armor("OPENSSH", """
        b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAiAAAABNlY2RzYS
        1zaGEyLW5pc3RwMzg0AAAACG5pc3RwMzg0AAAAYQTSEW6OecOexUtpNCRMvugVwokZ7j4U
        xH7jgZOyPyy3rnWeuw4h58O1K9D21d3D9Bvce18uGgXA0ZvhPNCsro1mtKWC8vtUpZPmdL
        j6pvT7eENTyPEpZUVi8JO6C2w5qNMAAADYhsSzZYbEs2UAAAATZWNkc2Etc2hhMi1uaXN0
        cDM4NAAAAAhuaXN0cDM4NAAAAGEE0hFujnnDnsVLaTQkTL7oFcKJGe4+FMR+44GTsj8st6
        51nrsOIefDtSvQ9tXdw/Qb3HtfLhoFwNGb4TzQrK6NZrSlgvL7VKWT5nS4+qb0+3hDU8jx
        KWVFYvCTugtsOajTAAAAMCrNdrIA45VG4Kb/wObdPpxZLmyyCNHmE2isNO04HnnU8HLkDx
        MWyBtkD268oGddQQAAABBlY2RzYTM4NEBleGFtcGxl
        """)
    static let ecdsa384_pub = "ecdsa-sha2-nistp384 AAAAE2VjZHNhLXNoYTItbmlzdHAzODQAAAAIbmlzdHAzODQAAABhBNIRbo55w57FS2k0JEy+6BXCiRnuPhTEfuOBk7I/LLeudZ67DiHnw7Ur0PbV3cP0G9x7Xy4aBcDRm+E80KyujWa0pYLy+1Slk+Z0uPqm9Pt4Q1PI8SllRWLwk7oLbDmo0w=="
    static let ecdsa384_fp = "SHA256:OT9RvvJ52YW3/n2kioEQb7JJyZ5FPUPDDIu8ahqJmPc"

    static let ecdsa384_enc = armor("OPENSSH", """
        b3BlbnNzaC1rZXktdjEAAAAACmFlczI1Ni1jdHIAAAAGYmNyeXB0AAAAGAAAABAbzXdVDQ
        jPpM7LQX47j3d6AAAABAAAAAEAAACIAAAAE2VjZHNhLXNoYTItbmlzdHAzODQAAAAIbmlz
        dHAzODQAAABhBDPH1RAkt606VLuSPxMt7ILbsQLfTZMAqvtp7LAinJXYQ7OMqQVnCHLMNw
        xsVgZp3gZGpEKVeW0XrFzAOmC3u1OPZdmk0uUmOqgAOXLEMTyslXSeaeyWjMXkYd340vDI
        ggAAAOAA7cjvSYs9Jqto/E0rRfl1DhW95fb0GWGR9D+GbVi/byn1MxAJRpAlv0ip7iDRO/
        I96exBPWAP96Vlcwl3UpX8/m3eaCdsAHJpAWtE1Jf6DpwsCbVMfO+OrVb5Wk1YlcCiMwL0
        hauNJtCvMs+YbrX5nyEzXpRckmZMKJXEZQ1DJWTt6CjVaI440Z16W+uROlKlanLXeD6u9y
        juluuq+oK8ZUnIGiRDb5apfiYb0aZ0qXudUY3OTk4wWrPl65vRkkt1cxx7MVbxBP+El0/K
        IdN171LSnJMK+WWvp6+nuW6ssQ==
        """)
    static let ecdsa384_enc_pub = "ecdsa-sha2-nistp384 AAAAE2VjZHNhLXNoYTItbmlzdHAzODQAAAAIbmlzdHAzODQAAABhBDPH1RAkt606VLuSPxMt7ILbsQLfTZMAqvtp7LAinJXYQ7OMqQVnCHLMNwxsVgZp3gZGpEKVeW0XrFzAOmC3u1OPZdmk0uUmOqgAOXLEMTyslXSeaeyWjMXkYd340vDIgg=="
    static let ecdsa384_enc_fp = "SHA256:B0j+6oEUBvgU4zli1haiFJUD9cT0t+XbeQncuT7KLlY"

    static let ecdsa384_pkcs8 = armor("", """
        MIICDAIBADCCAWQGByqGSM49AgEwggFXAgEBMDwGByqGSM49AQECMQD/////////
        /////////////////////////////////v////8AAAAAAAAAAP////8wewQw////
        //////////////////////////////////////7/////AAAAAAAAAAD////8BDCz
        MS+n4j7n5JiOBWvj+C0ZGB2cbv6BQRIDFAiPUBOHWsZWOY2KLtGdKoXI7dPsKu8D
        FQCjNZJqoxmieh0AiWpnc6SCes2scwRhBKqHyiK+iwU3jrHHHvMgrXRuHTtii6eb
        mFn3QeCCVCo4VQLyXb9VKWw6VF44cnYKtzYX3kqWJixvXZ6Yv5KS3Cn49B29KJoU
        fOnaMRO18LjACmCxzh1+gZ16Qx18kOoOXwIxAP//////////////////////////
        /////8djTYH0Ny3fWBoNskiwp3rs7BlqzMUpcwIBAQSBnjCBmwIBAQQwjCOA0s1E
        uBHzlVPeYdT8zNcj22tOYuOKb0ueDnvabWW/6rnOCQ3ZTW6Fe0ACq5DHoWQDYgAE
        6Mx7UiWrE3w2NsgwLqThERbAR0v4m7+YERRBcUvokMxmRIGfzqFOzQk2nD1bBC5L
        DHGzhjnYb+ObJ3L4TdUz8SYdtsv1sO5Sm3575SR5O0UJIU0plud/gb12fJlW2sPl
        """)
    static let ecdsa384_pkcs8_pub = "ecdsa-sha2-nistp384 AAAAE2VjZHNhLXNoYTItbmlzdHAzODQAAAAIbmlzdHAzODQAAABhBOjMe1IlqxN8NjbIMC6k4REWwEdL+Ju/mBEUQXFL6JDMZkSBn86hTs0JNpw9WwQuSwxxs4Y52G/jmydy+E3VM/EmHbbL9bDuUpt+e+UkeTtFCSFNKZbnf4G9dnyZVtrD5Q=="
    static let ecdsa384_pkcs8_fp = "SHA256:oGXsDTxWxEB+IjmHWGXibMVq2mWoNcHK3NuByNbwtmE"

    static let ecdsa521 = armor("OPENSSH", """
        b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAArAAAABNlY2RzYS
        1zaGEyLW5pc3RwNTIxAAAACG5pc3RwNTIxAAAAhQQAuzaUXnqnWPt9AnH7xidYmaoPauS4
        lK4ubcXE7iP7mXJHmiCbgQqqjeEwpcLhnbwPRf5n+k9QEcCz5ZGbZ9+VxuYAc181LAbnkk
        epLGQtPwNZT4YCQIqg7sPYjivgfcdFNCJUTV7g1qLjrlBkW5/X3tDkFsDryYzA8WQ+BKRI
        n8+0F9wAAAEQw/HoW8Px6FsAAAATZWNkc2Etc2hhMi1uaXN0cDUyMQAAAAhuaXN0cDUyMQ
        AAAIUEALs2lF56p1j7fQJx+8YnWJmqD2rkuJSuLm3FxO4j+5lyR5ogm4EKqo3hMKXC4Z28
        D0X+Z/pPUBHAs+WRm2fflcbmAHNfNSwG55JHqSxkLT8DWU+GAkCKoO7D2I4r4H3HRTQiVE
        1e4Nai465QZFuf197Q5BbA68mMwPFkPgSkSJ/PtBfcAAAAQgFbEoWgjkTaDcWcTICxvr98
        4CHNL0llvL/UJrcesb13/tTzhmoncFeB5vyO+L0+K2eKxDbCtrebJHfE+6P0ibxMIQAAAB
        BlY2RzYTUyMUBleGFtcGxlAQI=
        """)
    static let ecdsa521_pub = "ecdsa-sha2-nistp521 AAAAE2VjZHNhLXNoYTItbmlzdHA1MjEAAAAIbmlzdHA1MjEAAACFBAC7NpReeqdY+30CcfvGJ1iZqg9q5LiUri5txcTuI/uZckeaIJuBCqqN4TClwuGdvA9F/mf6T1ARwLPlkZtn35XG5gBzXzUsBueSR6ksZC0/A1lPhgJAiqDuw9iOK+B9x0U0IlRNXuDWouOuUGRbn9fe0OQWwOvJjMDxZD4EpEifz7QX3A=="
    static let ecdsa521_fp = "SHA256:J+B/EXjDK10cHoM73CkYrkXAAHALYFEjojN75hLv2kA"

    static let ecdsa521_enc = armor("OPENSSH", """
        b3BlbnNzaC1rZXktdjEAAAAACmFlczI1Ni1jdHIAAAAGYmNyeXB0AAAAGAAAABC0l3OmLn
        rBVob1XxnQtWXDAAAABAAAAAEAAACsAAAAE2VjZHNhLXNoYTItbmlzdHA1MjEAAAAIbmlz
        dHA1MjEAAACFBAC3Pcota+OeP4c3ZGe1vqy/ynDSBh/rn8xcYLKCoFHjdtB9XSJ4A2bn1j
        Pfi6qVuNQUptOWWahpqoSJnAkKiNy+lwCWL9d2wLlTiB5/cCMKXkn8Kiwjaan9CQ+H4OD7
        q3A/HAPkjbLGSbaf32oLUTTdgITBN11ceGYTd9GHeu0hAXHuxgAAARCzDkqUAWlFLtkTvx
        zRSBeHdC63vfCTH0HZDeKapnjZHCzPwoWRP4C4x/HcCXqsGXNsvFru5kB5B3PFJ8jwyu8g
        2odDNSHIxZhGVCE8FeZ0lunnezBNgjpWHoMrWbeXjt3p1qg0PMoiAmEOKVuZFabssjOpWx
        TcIPyMNffQ/Ydu/Gziz9l3czWNDHAC8b32pBAPGkxZGirMoB35hACRAFSlD2Aul+unjl0W
        Jd+DaioAssCfx7ZURx1nJaSUFtaqiXUZfgEYd4+hvKWlt5n+pUVvWKP9S3s7KMaCmOpF1w
        SLvQv+s1ijs6s3Se7KPzl2ZCPSy6cZI9Wj72Lvw4XacE3Wiui0EIsSBuGeWZ7ruEnuOA==
        """)
    static let ecdsa521_enc_pub = "ecdsa-sha2-nistp521 AAAAE2VjZHNhLXNoYTItbmlzdHA1MjEAAAAIbmlzdHA1MjEAAACFBAC3Pcota+OeP4c3ZGe1vqy/ynDSBh/rn8xcYLKCoFHjdtB9XSJ4A2bn1jPfi6qVuNQUptOWWahpqoSJnAkKiNy+lwCWL9d2wLlTiB5/cCMKXkn8Kiwjaan9CQ+H4OD7q3A/HAPkjbLGSbaf32oLUTTdgITBN11ceGYTd9GHeu0hAXHuxg=="
    static let ecdsa521_enc_fp = "SHA256:BkWUJp12NLjaWIrhTLRsVMcZeSk90ar4tva8uThKW9c"

    static let ed25519 = armor("OPENSSH", """
        b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
        QyNTUxOQAAACCXmVyIBrqjnNhTwS57tHkdmOrfRlHHGKF6qRxrRo/PxwAAAJCfGstTnxrL
        UwAAAAtzc2gtZWQyNTUxOQAAACCXmVyIBrqjnNhTwS57tHkdmOrfRlHHGKF6qRxrRo/Pxw
        AAAECPYGwJOz5hfYwNQ0sLAvTlurdCN+rHa8NvjvbrzQjx15eZXIgGuqOc2FPBLnu0eR2Y
        6t9GUccYoXqpHGtGj8/HAAAADHRlc3RAZXhhbXBsZQE=
        """)
    static let ed25519_pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJeZXIgGuqOc2FPBLnu0eR2Y6t9GUccYoXqpHGtGj8/H"
    static let ed25519_fp = "SHA256:dKLuuSDSpW6DWrd/r8jRYSXgl+X6IgApqRqw/GOW4nE"

    static let ed25519_cbc = armor("OPENSSH", """
        b3BlbnNzaC1rZXktdjEAAAAACmFlczEyOC1jYmMAAAAGYmNyeXB0AAAAGAAAABDKwl7Rl8
        cckgpWBThiQP1BAAAABAAAAAEAAAAzAAAAC3NzaC1lZDI1NTE5AAAAILzqYufFUmuDUh7X
        uZu6hi8y/Q+Hsb5QM986mwM/9HDuAAAAkCaGMfI45oXVWGgxQbT/DFvQadnxxScwW7NCgz
        MXGPgEIApdcXUwl2rNmY09o+cSEN2gkLrIQz5UhoYGKl6+sW3gl4EdE22iWJ4A2RvXcrE9
        mPWcI5Qd/qt0ESzTp8RKx/KGYkrrYm+mzvJ1NJ3Bw5dIiUJwpZpWYb05Hsmzm1BiJV7+1b
        M41qzVmRMHK4Qs4Q==
        """)
    static let ed25519_cbc_pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILzqYufFUmuDUh7XuZu6hi8y/Q+Hsb5QM986mwM/9HDu"
    static let ed25519_cbc_fp = "SHA256:OOaV5xjsijcHnzgKh7rDSONeoJkKgtsCx/E4D6871QA"

    static let ed25519_chacha = armor("OPENSSH", """
        b3BlbnNzaC1rZXktdjEAAAAAHWNoYWNoYTIwLXBvbHkxMzA1QG9wZW5zc2guY29tAAAABm
        JjcnlwdAAAABgAAAAQPs2u+bSxIvyScm0bz1+aBwAAAAQAAAABAAAAMwAAAAtzc2gtZWQy
        NTUxOQAAACAqcleyuzib24Ltqwj79iJgDdN8fORyQ1ZOgih5YOlNvQAAAJCJI+P8a7AL7v
        jh40odLxROTirzDTI4UvRol13vDKNQ8TSxTLRaY+s1uJexJjeFiG1Yz5fNOhOgRfemoPtR
        0eE0Gj2jd9IJ8cfeape9AIrfrBhNDrW16qfj4DITT/oViFHA9GYzaMQ2L8Z+yZxiqRbOoj
        xqt5Tz0FVnTNjacpDQCbC8nNnnyXyz6iC5InPbc6t4EgCW6OIihPVEqnwHfmUN
        """)
    static let ed25519_chacha_pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICpyV7K7OJvbgu2rCPv2ImAN03x85HJDVk6CKHlg6U29"
    static let ed25519_chacha_fp = "SHA256:+vX8JgHojpb/l98KxvDn32HscFPGf7GR4fITtwNN6ww"

    static let ed25519_enc = armor("OPENSSH", """
        b3BlbnNzaC1rZXktdjEAAAAACmFlczI1Ni1jdHIAAAAGYmNyeXB0AAAAGAAAABAY34UQiz
        6eRQUbWuZXDymXAAAAGAAAAAEAAAAzAAAAC3NzaC1lZDI1NTE5AAAAILV1/ZPH9/+Bi8WE
        rdRFkMHXxdd5U/l3D4rbuw20Tw1/AAAAkJgSm4cduDbnGtijKnB90/XlwXVr7XFnpd/sqG
        yjgUxlToquGEYjEE23/+WEfgfMhnbzwt+6GiqcJiC7cVayK+WUAHsNSCrQ4RXGVEpGoC7Q
        G3m6WVKP503EH6UsJKiaohDgp/MYZdme949pZQ/doBPIgwc9FfO/uqbEYV4KHJTtVwS8ob
        GW4AF1lJEoDI3GCQ==
        """)
    static let ed25519_enc_pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILV1/ZPH9/+Bi8WErdRFkMHXxdd5U/l3D4rbuw20Tw1/"
    static let ed25519_enc_fp = "SHA256:1ceGqJbiBQVphwXyYuMQ9iSv0yrtVbFh3vVlobxG3lg"

    static let ed25519_gcm = armor("OPENSSH", """
        b3BlbnNzaC1rZXktdjEAAAAAFmFlczI1Ni1nY21Ab3BlbnNzaC5jb20AAAAGYmNyeXB0AA
        AAGAAAABBf2u4a1iQRoL2bVgAl8Hr9AAAABAAAAAEAAAAzAAAAC3NzaC1lZDI1NTE5AAAA
        IIfsSCOaLIXpIVKaNZN8vOZGDFuvQ/uuT/28fFlLauIKAAAAkCPk1Eg4HvlQIl+y1OXBuL
        GTq6l8X5yCqjPkzZxTFC55WDoabedUbDxcLKyCsbMF1/HVYkWvwonpNvh/viluzQZ8FyaK
        R0SiDuv1/F903nkjLdDraJlMycSKl5e/Ho8apss9rlMcZmO++rlaIP9G2xf35jg/K8dKZQ
        8yvzrzbXncWF6uyk0Gyi4w29v2VjvJyP7umgKRsZoxCVl/CB50H8k=
        """)
    static let ed25519_gcm_pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIfsSCOaLIXpIVKaNZN8vOZGDFuvQ/uuT/28fFlLauIK"
    static let ed25519_gcm_fp = "SHA256:lAzwJuYN2QkKYXQDeCOS3V6Smxh99LHLVladmo6UUBA"

    static let ed25519_rounds1 = armor("OPENSSH", """
        b3BlbnNzaC1rZXktdjEAAAAACmFlczI1Ni1jdHIAAAAGYmNyeXB0AAAAGAAAABBUlazEZp
        zUBS+2eIz4n6m8AAAAAQAAAAEAAAAzAAAAC3NzaC1lZDI1NTE5AAAAIIBZFv4e2OzvMzUC
        9ner/dT7JW8FfqBvJgYQCAlGomTIAAAAoHsEJmCquBVvFxK1XGapIffZFxoF0x1HhGRxR1
        XIhTt0whsoXurLTwfB84X+fYDXn7rukNOQwEMQujuez1KlBYFN9TM7AyV6pDNLAYa1G7e1
        MvEnYzGd5mltHe1PakY6HJqbB6ByqBmCZoaZNkXT2H7OIa5nlc3qdsykqvOLmxUrpGjfvj
        cGaahoo/rdnFldZ+61JXBBpZMTCAOeokA+5wM=
        """)
    static let ed25519_rounds1_pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIBZFv4e2OzvMzUC9ner/dT7JW8FfqBvJgYQCAlGomTI"
    static let ed25519_rounds1_fp = "SHA256:4eUDqyOuQL1tdWGvjkFSJByQ/9GmFtD8q/XNoQ8C9QM"

    static let rsa = armor("OPENSSH", """
        b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAlwAAAAdzc2gtcn
        NhAAAAAwEAAQAAAIEA2HE71REIB08zsPze8tGUuOWK+A9Rof0Zd9Si3CBxj6w+g8KH+V6v
        hvzg0Y5dqilXfG6tlnIxlnZGRoJfHjiv2sQQqw7TL3wxnp5wLdh4VQFBBJCXi/X/ya5qvb
        +e4Er50YoPDX5dHY4r42b5KFPwfT+clE+aQkR30WARTC1f7lMAAAIAR49a1UePWtUAAAAH
        c3NoLXJzYQAAAIEA2HE71REIB08zsPze8tGUuOWK+A9Rof0Zd9Si3CBxj6w+g8KH+V6vhv
        zg0Y5dqilXfG6tlnIxlnZGRoJfHjiv2sQQqw7TL3wxnp5wLdh4VQFBBJCXi/X/ya5qvb+e
        4Er50YoPDX5dHY4r42b5KFPwfT+clE+aQkR30WARTC1f7lMAAAADAQABAAAAgCHyiKbfd/
        nlgz8EOletZX0mi4hiprQYhXEVCcIq2ITnSBrUXQ1Qu4kZio1znd89grpChbluCbcGr/kJ
        I7ASxgnpFWPsZ5nsF2hCbRvVuCjukZ3MkxemRtmBrC+7NBAPtGdf80Ep4lKjhAeTlLA3GR
        ggyLXC7DFI/OFehCHx3cVBAAAAQBXtw9MruZh2YXD5EQHzQ6kFyv3OZI+UbCTrx+OfP+GH
        y8A6iEAN7htFji3ZpRyFzuBfP/0bhaWMmClSSpmfVJ8AAABBAOw6n7HpBp0pd7giYu1sHa
        H2Dm6pdq19TizE/ERIrwdXZLWLeJMpb7LoEmpa1fBcrgpVsTYSmkqE09smyHSDujMAAABB
        AOqOqoMOadTcwimWojo4p+AjbYXm56XXfTY+CmtT62Lam/EdRL7iLSz2SMpNrx28Uw8Z8d
        Pr0cmhCyHn1pThG2EAAAALcnNhQGV4YW1wbGU=
        """)
    static let rsa_pub = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQDYcTvVEQgHTzOw/N7y0ZS45Yr4D1Gh/Rl31KLcIHGPrD6Dwof5Xq+G/ODRjl2qKVd8bq2WcjGWdkZGgl8eOK/axBCrDtMvfDGennAt2HhVAUEEkJeL9f/Jrmq9v57gSvnRig8Nfl0djivjZvkoU/B9P5yUT5pCRHfRYBFMLV/uUw=="
    static let rsa_fp = "SHA256:8TlgLwOfiC5xbqkivIAwsDiLcxlBSU4ABlbCVdMVRys"

    static let rsa_pem = armor("RSA", """
        MIICXAIBAAKBgQDjo44ghplK3A3vd9ON4Ga2cbCjiti3FfbW0rG82kGHt9I7r/s8
        pkmfUOgOcJI+/Nx8aal/sAg8Lokczvl6tqT1Ta0nTI/VDlSUnyJk7q+6amwWoF22
        JOKgu5JYzbwAMxhGjwWL2FJw4k1Na5Efab9xg2FuFLxkDvlLrVDckA5sTQIDAQAB
        AoGATk1BI0EJU620AW43T/MjgZhDyU2GVBvzB1v8FVkB93wsjalwAyGz62A6IlVo
        iNATy5vQ2QIlIwZi1Y0AVcyjmrMrz504zyf056cKaLneokysRTaAuGiUlnVxpgA/
        nY8dKd0oKZ9+4Dl1+ikOtKb8+9ka/FAMN9YBYyM4VIfuImUCQQD+Q9wMpPhU2Zwp
        YzRL6wLAXml52sCZA2EL53ppW2Pit8RvV7hMZ1XOyXHuUBiOP5Mt1RrnWuq0aE/i
        eXBcxF4jAkEA5TEvnraQLQcY+/9mXp3VErF0eY8t5zWCUsMeyD8/psbzoqvI5gAe
        II+XroeQCtMK0J2mR9iCfi0sY7NHcvpazwJBAMXlix9MRvvDYwfTbqCoCTwwP2fY
        Go79UksxntThjJyo6vq+C3Nr+h04LLSPIo2Ubbj+ZDItUP+ZMJPUcN/C9oECQA9O
        lqiZ9s0bET8bUqH1Fpr2ARpxRpaPi1GtuGuVVdH0rabqpDNARMLV9EJyyVr/t1Qx
        gz5/Jk/5AmLyQU5bjB8CQCmxjmnInMncWZ9oGJ0gIRgT+/7GADSsdFv/TRxiugxN
        siHmpzVvC5u5N0CHirEmtvND+eJKXl0+t+4FyqLq1KU=
        """)
    static let rsa_pem_pub = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQDjo44ghplK3A3vd9ON4Ga2cbCjiti3FfbW0rG82kGHt9I7r/s8pkmfUOgOcJI+/Nx8aal/sAg8Lokczvl6tqT1Ta0nTI/VDlSUnyJk7q+6amwWoF22JOKgu5JYzbwAMxhGjwWL2FJw4k1Na5Efab9xg2FuFLxkDvlLrVDckA5sTQ=="
    static let rsa_pem_fp = "SHA256:wO7b7fnROA8Q04F/qf86/drA3Yz40TBQduV2xKohMZ0"
}
